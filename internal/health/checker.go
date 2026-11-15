// Active health checking. A Checker probes targets on a fixed interval and
// feeds outcomes into a Registry. Probes are bounded in concurrency and time,
// never follow redirects, and are cancelled with the gateway context.
package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// Target is a backend exposed to the checker. ID is stable across reloads.
type Target struct {
	ID  string
	URL *url.URL
}

const maxProbeConcurrency = 8

// Checker actively probes targets and records outcomes in a Registry.
type Checker struct {
	cfg      config.HealthCheck
	targets  []Target
	registry *Registry
	client   *http.Client
	logger   *slog.Logger
	onProbe  func(ProbeResult)
}

// NewChecker builds a checker from validated health config. Missing values get
// the same defaults as config loading so direct construction stays safe.
func NewChecker(cfg config.HealthCheck, targets []Target, registry *Registry, client *http.Client, logger *slog.Logger, onProbe func(ProbeResult)) (*Checker, error) {
	if registry == nil {
		return nil, fmt.Errorf("health: nil registry")
	}
	if cfg.Path == "" {
		cfg.Path = "/healthz"
	}
	if !strings.HasPrefix(cfg.Path, "/") {
		return nil, fmt.Errorf("health: path %q must start with /", cfg.Path)
	}
	if cfg.Interval.Duration <= 0 {
		cfg.Interval.Duration = 5 * time.Second
	}
	if cfg.Timeout.Duration <= 0 {
		cfg.Timeout.Duration = time.Second
	}
	if cfg.HealthyThreshold < 1 {
		cfg.HealthyThreshold = 2
	}
	if cfg.UnhealthyThreshold < 1 {
		cfg.UnhealthyThreshold = 3
	}
	if cfg.ExpectedStatusMin == 0 {
		cfg.ExpectedStatusMin = 200
	}
	if cfg.ExpectedStatusMax == 0 {
		cfg.ExpectedStatusMax = 399
	}
	if cfg.ExpectedStatusMin < 100 || cfg.ExpectedStatusMax > 599 || cfg.ExpectedStatusMin > cfg.ExpectedStatusMax {
		return nil, fmt.Errorf("health: invalid expected status range %d-%d", cfg.ExpectedStatusMin, cfg.ExpectedStatusMax)
	}
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if t.ID == "" || t.URL == nil {
			return nil, fmt.Errorf("health: target with empty id or url")
		}
		if t.URL.Scheme != "http" && t.URL.Scheme != "https" {
			return nil, fmt.Errorf("health: target %q: scheme must be http or https", t.ID)
		}
		if t.URL.User != nil || t.URL.RawQuery != "" || t.URL.Fragment != "" {
			return nil, fmt.Errorf("health: target %q: userinfo, query and fragment are not allowed", t.ID)
		}
		if _, dup := seen[t.ID]; dup {
			return nil, fmt.Errorf("health: duplicate target %q", t.ID)
		}
		seen[t.ID] = struct{}{}
	}
	if client == nil {
		client = defaultClient()
	} else {
		// A shared client must not make health probes follow redirects, which
		// could mask a broken backend or leave the target URL family.
		clone := *client
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &clone
	}
	// Registering here prevents a checker from silently reporting into a
	// registry that never learned about its targets. Server wiring still
	// registers targets when active checks are disabled.
	for _, t := range targets {
		registry.Register(t.ID)
	}
	return &Checker{
		cfg:      cfg,
		targets:  targets,
		registry: registry,
		client:   client,
		logger:   logger,
		onProbe:  onProbe,
	}, nil
}

func defaultClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ProbeOnce probes every target once, with at most maxProbeConcurrency probes
// in flight. It returns after all probes finish or ctx is cancelled.
func (c *Checker) ProbeOnce(ctx context.Context) {
	if len(c.targets) == 0 || ctx.Err() != nil {
		return
	}
	jobs := make(chan Target, len(c.targets))
	for _, t := range c.targets {
		jobs <- t
	}
	close(jobs)

	workers := maxProbeConcurrency
	if len(c.targets) < workers {
		workers = len(c.targets)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				c.probe(ctx, t)
			}
		}()
	}
	wg.Wait()
}

// Run probes immediately and then every Interval until ctx is cancelled.
// Disabled health checks return without doing work.
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled || len(c.targets) == 0 || ctx.Err() != nil {
		return
	}
	c.ProbeOnce(ctx)
	ticker := time.NewTicker(c.cfg.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.ProbeOnce(ctx)
		}
	}
}

// CloseIdleConnections releases pooled probe connections during shutdown.
func (c *Checker) CloseIdleConnections() { c.client.CloseIdleConnections() }

func (c *Checker) probe(parent context.Context, t Target) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout.Duration)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.probeURL(t), nil)
	if err != nil {
		c.record(t.ID, ProbeResult{Target: t.ID, Err: err})
		return
	}
	req.Header.Set("User-Agent", "api-gateway-health/1.0")
	req.Header.Set("Accept", "*/*")

	start := time.Now()
	resp, err := c.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		// Parent cancellation means gateway shutdown, not a sick target.
		if parent.Err() != nil {
			return
		}
		c.record(t.ID, ProbeResult{Target: t.ID, Duration: elapsed, Err: err})
		return
	}
	// Drain a little so the connection can be reused, then close.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	healthy := resp.StatusCode >= c.cfg.ExpectedStatusMin && resp.StatusCode <= c.cfg.ExpectedStatusMax
	result := ProbeResult{
		Target:     t.ID,
		Healthy:    healthy,
		StatusCode: resp.StatusCode,
		Duration:   elapsed,
	}
	if !healthy {
		result.Err = fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	c.record(t.ID, result)
}

func (c *Checker) record(target string, result ProbeResult) {
	if result.Healthy {
		c.registry.Success(target)
	} else {
		reason := "probe failed"
		if result.Err != nil {
			reason = result.Err.Error()
		}
		c.registry.Failure(target, reason)
	}
	if c.onProbe != nil {
		c.onProbe(result)
	}
	if c.logger == nil {
		return
	}
	if result.Healthy {
		c.logger.Debug("health probe ok",
			slog.String("target", target),
			slog.Int("status", result.StatusCode),
			slog.Duration("duration", result.Duration))
		return
	}
	attrs := []any{
		slog.String("target", target),
		slog.Duration("duration", result.Duration),
	}
	if result.StatusCode != 0 {
		attrs = append(attrs, slog.Int("status", result.StatusCode))
	}
	if result.Err != nil {
		attrs = append(attrs, slog.String("error", result.Err.Error()))
	}
	c.logger.Debug("health probe failed", attrs...)
}

func (c *Checker) probeURL(t Target) string {
	u := *t.URL
	u.Path = strings.TrimSuffix(u.Path, "/") + c.cfg.Path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
