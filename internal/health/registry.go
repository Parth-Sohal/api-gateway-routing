// Package health tracks target availability. A passive registry applies
// hysteresis to real request outcomes; the active Checker probes configured
// targets on an interval. Both feed the same registry, so the balancer only
// needs one question answered: is this target available right now?
//
// The registry starts optimistically available. Ejection happens after
// UnhealthyThreshold consecutive failures and recovery after
// HealthyThreshold consecutive successes, which absorbs single blips without
// hiding a genuinely dead backend.
package health

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

// Change describes an availability transition.
type Change struct {
	Target    string
	Available bool
	Reason    string
	At        time.Time
}

// ProbeResult is emitted for every active probe (success or failure) so
// metrics and logs can observe probe latency without the checker importing
// either package.
type ProbeResult struct {
	Target     string
	Healthy    bool
	StatusCode int
	Duration   time.Duration
	Err        error
}

// Status is an admin/metrics view of one target.
type Status struct {
	Target               string
	Available            bool
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	LastChange           time.Time
	LastProbe            time.Time
	LastError            string
}

type entry struct {
	available  bool
	failures   int
	successes  int
	lastChange time.Time
	lastProbe  time.Time
	lastError  string
}

// Registry is a concurrency-safe availability table. The target set is
// normally registered once during state construction; unknown targets read as
// available and unknown reports are ignored so a bug cannot grow the map.
type Registry struct {
	healthyThreshold   int
	unhealthyThreshold int
	clock              clock.Clock
	onChange           func(Change)

	mu      sync.RWMutex
	entries map[string]*entry
}

// NewRegistry builds a registry. Non-positive thresholds fall back to 2 and 3.
// A nil clock uses time.Now and a nil hook is ignored.
func NewRegistry(healthyThreshold, unhealthyThreshold int, c clock.Clock, onChange func(Change)) *Registry {
	if healthyThreshold < 1 {
		healthyThreshold = 2
	}
	if unhealthyThreshold < 1 {
		unhealthyThreshold = 3
	}
	if c == nil {
		c = clock.Real{}
	}
	return &Registry{
		healthyThreshold:   healthyThreshold,
		unhealthyThreshold: unhealthyThreshold,
		clock:              c,
		onChange:           onChange,
		entries:            make(map[string]*entry),
	}
}

// Register adds a target if absent. Existing state is preserved.
func (r *Registry) Register(target string) {
	if target == "" {
		return
	}
	r.mu.Lock()
	if _, ok := r.entries[target]; !ok {
		now := r.clock.Now()
		r.entries[target] = &entry{available: true, lastChange: now}
	}
	r.mu.Unlock()
}

// Available reports target health. Unknown targets are treated as available.
func (r *Registry) Available(target string) bool {
	r.mu.RLock()
	e, ok := r.entries[target]
	if !ok {
		r.mu.RUnlock()
		return true
	}
	up := e.available
	r.mu.RUnlock()
	return up
}

// Success records a healthy request outcome and may revive a down target.
func (r *Registry) Success(target string) {
	r.report(target, true, "")
}

// Failure records an unhealthy request outcome and may eject a target.
func (r *Registry) Failure(target string, reason string) {
	r.report(target, false, reason)
}

func (r *Registry) report(target string, success bool, reason string) {
	now := r.clock.Now()
	var change *Change

	r.mu.Lock()
	e, ok := r.entries[target]
	if !ok {
		r.mu.Unlock()
		return
	}
	e.lastProbe = now
	if success {
		if e.successes < r.healthyThreshold {
			e.successes++
		}
		e.failures = 0
		if !e.available && e.successes >= r.healthyThreshold {
			e.available = true
			e.successes = 0
			e.lastChange = now
			e.lastError = ""
			change = &Change{Target: target, Available: true, Reason: "healthy_threshold", At: now}
		}
	} else {
		e.successes = 0
		if e.failures < r.unhealthyThreshold {
			e.failures++
		}
		e.lastError = truncate(reason, 200)
		if e.available && e.failures >= r.unhealthyThreshold {
			e.available = false
			e.lastChange = now
			change = &Change{Target: target, Available: false, Reason: "unhealthy_threshold", At: now}
		}
	}
	r.mu.Unlock()

	if change != nil && r.onChange != nil {
		r.onChange(*change)
	}
}

// Snapshot returns a stable, sorted view for the admin API and metrics.
func (r *Registry) Snapshot() []Status {
	r.mu.RLock()
	out := make([]Status, 0, len(r.entries))
	for id, e := range r.entries {
		out = append(out, Status{
			Target:               id,
			Available:            e.available,
			ConsecutiveFailures:  e.failures,
			ConsecutiveSuccesses: e.successes,
			LastChange:           e.lastChange,
			LastProbe:            e.lastProbe,
			LastError:            e.lastError,
		})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// Targets returns the registered target IDs in sorted order.
func (r *Registry) Targets() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.entries))
	for id := range r.entries {
		out = append(out, id)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}
