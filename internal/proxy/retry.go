package proxy

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

const defaultBodyBuffer = 64 << 10

// retryPolicy is the immutable retry rule derived from config for one
// upstream.
type retryPolicy struct {
	enabled     bool
	maxAttempts int
	methods     map[string]struct{}
	statuses    map[int]struct{}
	onTimeout   bool
	onConn      bool
	baseBackoff time.Duration
	maxBackoff  time.Duration
	bodyBuffer  int64
}

func newRetryPolicy(c config.RetryConfig) retryPolicy {
	p := retryPolicy{
		enabled:     c.IsEnabled(),
		maxAttempts: c.MaxAttempts,
		methods:     make(map[string]struct{}, len(c.Methods)),
		statuses:    make(map[int]struct{}, len(c.On)),
		baseBackoff: c.BaseBackoff.Duration,
		maxBackoff:  c.MaxBackoff.Duration,
		bodyBuffer:  int64(c.BodyBufferBytes),
	}
	if p.maxAttempts < 1 {
		p.maxAttempts = 2
	}
	if p.maxAttempts > 10 {
		p.maxAttempts = 10
	}
	if len(c.Methods) == 0 {
		c.Methods = []string{"GET", "HEAD", "PUT", "DELETE", "OPTIONS"}
	}
	for _, m := range c.Methods {
		p.methods[strings.ToUpper(strings.TrimSpace(m))] = struct{}{}
	}
	if len(c.On) == 0 {
		c.On = []string{"connection_error", "502", "503", "504"}
	}
	for _, reason := range c.On {
		switch reason {
		case "connection_error":
			p.onConn = true
		case "timeout":
			p.onTimeout = true
		default:
			if code, err := strconv.Atoi(reason); err == nil && code >= 500 && code <= 599 {
				p.statuses[code] = struct{}{}
			}
		}
	}
	if p.bodyBuffer <= 0 {
		p.bodyBuffer = defaultBodyBuffer
	}
	return p
}

func (p retryPolicy) allowsMethod(method string) bool {
	if !p.enabled {
		return false
	}
	_, ok := p.methods[strings.ToUpper(method)]
	return ok
}

func (p retryPolicy) retryStatus(status int) bool {
	_, ok := p.statuses[status]
	return ok
}

func (p retryPolicy) retryError(timedOut, canceled bool) bool {
	if canceled {
		return false
	}
	if timedOut {
		return p.onTimeout
	}
	return p.onConn
}

func (p retryPolicy) backoff(attempt int) time.Duration {
	if p.baseBackoff <= 0 {
		return 0
	}
	d := p.baseBackoff
	for i := 1; i < attempt; i++ {
		if d >= p.maxBackoff && p.maxBackoff > 0 {
			return p.maxBackoff
		}
		d *= 2
		if d <= 0 {
			break
		}
	}
	if p.maxBackoff > 0 && d > p.maxBackoff {
		return p.maxBackoff
	}
	return d
}

// retryBudget is a token bucket that caps retries as a fraction of requests.
// Each request deposits budgetPercent/100 tokens (up to a burst); each retry
// costs one. A short initial burst lets a fresh process retry without waiting.
type retryBudget struct {
	mu         sync.Mutex
	tokens     float64
	burst      float64
	perRequest float64
}

func newRetryBudget(percent int) *retryBudget {
	if percent < 1 || percent > 100 {
		percent = 10
	}
	per := float64(percent) / 100
	burst := math.Ceil(1 / per)
	if burst < 3 {
		burst = 3
	}
	if burst > 100 {
		burst = 100
	}
	return &retryBudget{tokens: burst, burst: burst, perRequest: per}
}

func (b *retryBudget) deposit() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.tokens += b.perRequest
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.mu.Unlock()
}

func (b *retryBudget) allow() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
