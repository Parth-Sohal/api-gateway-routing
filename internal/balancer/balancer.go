// Package balancer selects a backend target for each proxy attempt. Pools are
// immutable after construction; only counters and smooth-WRR weights mutate and
// those are guarded by atomics or a short mutex that is never held across I/O.
//
// Selection always skips targets marked unhealthy by the health registry and
// targets excluded by the caller (retries must not hit an already-failed
// target). The returned Target carries an in-flight counter that the caller
// releases exactly once, which is what makes least_conn work.
package balancer

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// Strategy is a selection algorithm.
type Strategy string

const (
	// RoundRobin rotates through healthy targets.
	RoundRobin Strategy = "round_robin"
	// WeightedRoundRobin is smooth weighted round robin: weights are spread
	// evenly instead of producing bursts.
	WeightedRoundRobin Strategy = "weighted_round_robin"
	// LeastConn prefers the healthy target with the fewest in-flight attempts.
	LeastConn Strategy = "least_conn"
)

// ErrNoTargets means every target was unhealthy, excluded, or the pool is
// empty. Callers should map it to 503.
var ErrNoTargets = errors.New("balancer: no available targets")

// Health reports whether a target is currently available. Implementations must
// be safe for concurrent use. A nil Health treats every target as available.
type Health interface {
	Available(targetID string) bool
}

// Target is one backend instance inside a Pool. ID is the normalized target
// URL and is stable across config reloads.
type Target struct {
	ID     string
	URL    *url.URL
	Weight int

	inflight atomic.Int64
}

// Inflight returns the number of attempts currently using this target.
func (t *Target) Inflight() int64 { return t.inflight.Load() }

// Pool balances one configured upstream.
type Pool struct {
	name     string
	strategy Strategy
	targets  []*Target
	health   Health

	next atomic.Uint64

	mu      sync.Mutex // guards current
	current []int64    // smooth weighted round robin credit
}

// New builds a pool from validated upstream config. It re-validates the parts
// it depends on so it can be used in tests without the full config pipeline.
func New(name, strategy string, targets []config.Target, health Health) (*Pool, error) {
	s, err := ParseStrategy(strategy)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", name, err)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("upstream %q: no targets", name)
	}
	pool := &Pool{
		name:     name,
		strategy: s,
		health:   health,
		current:  make([]int64, len(targets)),
	}
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		u, err := url.Parse(t.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: target %q: %w", name, t.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("upstream %q: target %q: scheme must be http or https", name, t.URL)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("upstream %q: target %q: host is required", name, t.URL)
		}
		target := &Target{ID: u.String(), URL: u, Weight: t.Weight}
		if target.Weight <= 0 {
			target.Weight = 1
		}
		if _, dup := seen[target.ID]; dup {
			return nil, fmt.Errorf("upstream %q: duplicate target %q", name, target.ID)
		}
		seen[target.ID] = struct{}{}
		pool.targets = append(pool.targets, target)
	}
	return pool, nil
}

// ParseStrategy accepts config spellings and returns the canonical strategy.
func ParseStrategy(s string) (Strategy, error) {
	switch s {
	case "", "round_robin", "round-robin", "rr":
		return RoundRobin, nil
	case "weighted", "weighted_round_robin", "weighted-round-robin", "wrr":
		return WeightedRoundRobin, nil
	case "least_conn", "least-connections", "least_connections", "leastconn":
		return LeastConn, nil
	default:
		return "", fmt.Errorf("unknown balancer %q", s)
	}
}

// Name returns the upstream name.
func (p *Pool) Name() string { return p.name }

// Strategy returns the canonical selection strategy.
func (p *Pool) Strategy() Strategy { return p.strategy }

// Targets returns the immutable target list.
func (p *Pool) Targets() []*Target { return p.targets }

// Acquire selects a healthy, non-excluded target and increments its in-flight
// count. The caller must call Release with the returned target. exclude may be
// nil; IDs are target IDs previously attempted by this request.
func (p *Pool) Acquire(exclude map[string]struct{}) (*Target, error) {
	var t *Target
	switch p.strategy {
	case WeightedRoundRobin:
		t = p.pickWeighted(exclude)
	case LeastConn:
		t = p.pickLeastConn(exclude)
	default:
		t = p.pickRoundRobin(exclude)
	}
	if t == nil {
		return nil, fmt.Errorf("%w: upstream %q", ErrNoTargets, p.name)
	}
	t.inflight.Add(1)
	return t, nil
}

// Release decrements the in-flight count for an acquired target. Extra calls
// are clamped at zero so a proxy bug cannot corrupt least_conn accounting.
func (p *Pool) Release(t *Target) {
	if t == nil {
		return
	}
	for {
		n := t.inflight.Load()
		if n <= 0 {
			return
		}
		if t.inflight.CompareAndSwap(n, n-1) {
			return
		}
	}
}

func (p *Pool) eligible(t *Target, exclude map[string]struct{}) bool {
	if exclude != nil {
		if _, skip := exclude[t.ID]; skip {
			return false
		}
	}
	return p.health == nil || p.health.Available(t.ID)
}

func (p *Pool) pickRoundRobin(exclude map[string]struct{}) *Target {
	n := len(p.targets)
	start := int(p.next.Add(1)-1) % n
	for i := 0; i < n; i++ {
		t := p.targets[(start+i)%n]
		if p.eligible(t, exclude) {
			return t
		}
	}
	return nil
}

func (p *Pool) pickLeastConn(exclude map[string]struct{}) *Target {
	n := len(p.targets)
	start := int(p.next.Add(1)-1) % n
	var best *Target
	var bestLoad int64
	for i := 0; i < n; i++ {
		t := p.targets[(start+i)%n]
		if !p.eligible(t, exclude) {
			continue
		}
		load := t.inflight.Load()
		if best == nil || load < bestLoad {
			best, bestLoad = t, load
		}
	}
	return best
}

// pickWeighted implements smooth weighted round robin over the currently
// eligible targets. Credits of ineligible targets are reset so a recovered
// target does not get a burst of traffic.
func (p *Pool) pickWeighted(exclude map[string]struct{}) *Target {
	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		best     *Target
		bestIdx  = -1
		bestCred int64
		total    int64
	)
	for i, t := range p.targets {
		if !p.eligible(t, exclude) {
			p.current[i] = 0
			continue
		}
		p.current[i] += int64(t.Weight)
		total += int64(t.Weight)
		if best == nil || p.current[i] > bestCred {
			best, bestIdx, bestCred = t, i, p.current[i]
		}
	}
	if best == nil {
		return nil
	}
	p.current[bestIdx] -= total
	return best
}

// TargetSnapshot is an admin/metrics view of one target.
type TargetSnapshot struct {
	ID        string
	Weight    int
	Inflight  int64
	Available bool
}

// Snapshot returns per-target state. It is intended for admin endpoints and
// periodic metrics collection, not the request path.
func (p *Pool) Snapshot() []TargetSnapshot {
	out := make([]TargetSnapshot, 0, len(p.targets))
	for _, t := range p.targets {
		out = append(out, TargetSnapshot{
			ID:        t.ID,
			Weight:    t.Weight,
			Inflight:  t.inflight.Load(),
			Available: p.health == nil || p.health.Available(t.ID),
		})
	}
	return out
}
