// Package breaker implements a per-upstream circuit breaker with a rolling
// failure window and half-open probes. State is in-memory and per gateway
// instance: decisions are lock-free on the fast path and never touch Redis.
package breaker

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

// State is the breaker state. Values are stable for the Prometheus gauge:
// 0 closed, 1 half-open, 2 open.
type State int32

const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	default:
		return "unknown"
	}
}

// ErrIgnored marks an outcome that must not affect the breaker: client
// cancellation, client errors (4xx), or a request rejected locally.
var ErrIgnored = errors.New("breaker: ignored outcome")

// OpenError is returned when the breaker rejects a request.
type OpenError struct {
	RetryAfter time.Duration
	Reason     string
}

func (e *OpenError) Error() string { return "circuit breaker is open: " + e.Reason }

// Config controls trip and recovery behavior.
type Config struct {
	FailureThreshold  int
	FailureRatio      float64
	MinRequests       int
	Window            time.Duration
	Buckets           int
	OpenTimeout       time.Duration
	HalfOpenMaxProbes int
	HalfOpenSuccesses int
}

func (c Config) withDefaults() Config {
	if c.FailureThreshold < 1 {
		c.FailureThreshold = 5
	}
	if c.FailureRatio <= 0 || math.IsNaN(c.FailureRatio) {
		c.FailureRatio = 0.5
	}
	if c.FailureRatio > 1 {
		c.FailureRatio = 1
	}
	if c.MinRequests < 1 {
		c.MinRequests = 20
	}
	if c.Window <= 0 {
		c.Window = 10 * time.Second
	}
	if c.Buckets < 1 {
		c.Buckets = 10
	}
	if c.Buckets > 1000 {
		c.Buckets = 1000
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 5 * time.Second
	}
	if c.HalfOpenMaxProbes < 1 {
		c.HalfOpenMaxProbes = 3
	}
	if c.HalfOpenSuccesses < 1 {
		c.HalfOpenSuccesses = 2
	}
	return c
}

// Hooks observe transitions and rejections. They are called without the
// breaker lock held.
type Hooks struct {
	OnStateChange func(from, to State, reason string)
	OnRejected    func(reason string)
}

// Breaker guards one upstream.
type Breaker struct {
	cfg   Config
	clock clock.Clock
	hooks Hooks

	mu          sync.Mutex
	state       atomic.Int32
	window      *rollingWindow
	openedAt    time.Time
	consecutive int
	probes      int
	successes   int
}

// New builds a breaker. Nil clock uses time.Now.
func New(cfg Config, c clock.Clock, hooks Hooks) *Breaker {
	if c == nil {
		c = clock.Real{}
	}
	cfg = cfg.withDefaults()
	return &Breaker{
		cfg:    cfg,
		clock:  c,
		hooks:  hooks,
		window: newRollingWindow(cfg.Window, cfg.Buckets),
	}
}

// State returns the current state without locking.
func (b *Breaker) State() State { return State(b.state.Load()) }

// Allow decides whether an attempt may proceed. On success it returns a
// completion callback that must be called exactly once with nil (success),
// ErrIgnored (neutral), or a failure. Extra calls are ignored.
func (b *Breaker) Allow() (func(error), error) {
	if b.State() == Closed {
		return b.completion(b.recordClosed), nil
	}

	now := b.clock.Now()
	var tr *transition
	var reject *OpenError

	b.mu.Lock()
	switch State(b.state.Load()) {
	case Closed:
		// It recovered while we were waiting for the lock.
	case Open:
		remaining := b.cfg.OpenTimeout - now.Sub(b.openedAt)
		if remaining <= 0 {
			tr = b.transitionLocked(now, HalfOpen, "open_timeout")
		} else {
			reject = &OpenError{RetryAfter: remaining, Reason: "open"}
		}
	}
	if reject == nil && State(b.state.Load()) == HalfOpen {
		if b.probes >= b.cfg.HalfOpenMaxProbes {
			reject = &OpenError{RetryAfter: time.Second, Reason: "half_open_probes_exhausted"}
		} else {
			b.probes++
		}
	}
	b.mu.Unlock()

	b.notify(tr)
	if reject != nil {
		if b.hooks.OnRejected != nil {
			b.hooks.OnRejected(reject.Reason)
		}
		return nil, reject
	}
	if State(b.state.Load()) == HalfOpen {
		return b.completion(b.halfOpenDone), nil
	}
	return b.completion(b.recordClosed), nil
}

func (b *Breaker) completion(fn func(error)) func(error) {
	var once sync.Once
	return func(err error) { once.Do(func() { fn(err) }) }
}

// recordClosed records an outcome while the breaker is closed. Outcomes from
// requests admitted before a trip are ignored.
func (b *Breaker) recordClosed(err error) {
	if errors.Is(err, ErrIgnored) {
		return
	}
	now := b.clock.Now()
	success := err == nil

	var tr *transition
	b.mu.Lock()
	if State(b.state.Load()) != Closed {
		b.mu.Unlock()
		return
	}
	b.window.record(now, success)
	if success {
		b.consecutive = 0
	} else {
		b.consecutive++
		successes, failures := b.window.counts(now)
		total := successes + failures
		reason := ""
		if b.consecutive >= b.cfg.FailureThreshold {
			reason = "consecutive_failures"
		} else if total >= int64(b.cfg.MinRequests) && float64(failures)/float64(total) >= b.cfg.FailureRatio {
			reason = "failure_ratio"
		}
		if reason != "" {
			tr = b.transitionLocked(now, Open, reason)
		}
	}
	b.mu.Unlock()
	b.notify(tr)
}

// halfOpenDone resolves a half-open probe.
func (b *Breaker) halfOpenDone(err error) {
	now := b.clock.Now()
	var tr *transition
	b.mu.Lock()
	if State(b.state.Load()) != HalfOpen {
		b.mu.Unlock()
		return
	}
	if b.probes > 0 {
		b.probes--
	}
	switch {
	case errors.Is(err, ErrIgnored):
		// Neutral outcome: release the probe slot but do not count it.
	case err == nil:
		b.successes++
		if b.successes >= b.cfg.HalfOpenSuccesses {
			tr = b.transitionLocked(now, Closed, "probe_success")
		}
	default:
		tr = b.transitionLocked(now, Open, "probe_failure")
	}
	b.mu.Unlock()
	b.notify(tr)
}

// transitionLocked changes state. The caller must hold b.mu and call notify
// after unlocking.
func (b *Breaker) transitionLocked(now time.Time, to State, reason string) *transition {
	from := State(b.state.Load())
	if from == to {
		return nil
	}
	b.state.Store(int32(to))
	switch to {
	case Open:
		b.openedAt = now
		b.probes = 0
		b.successes = 0
	case HalfOpen:
		b.probes = 0
		b.successes = 0
		b.window.reset()
		b.consecutive = 0
	case Closed:
		b.openedAt = time.Time{}
		b.probes = 0
		b.successes = 0
		b.window.reset()
		b.consecutive = 0
	}
	return &transition{from: from, to: to, reason: reason}
}

func (b *Breaker) notify(tr *transition) {
	if tr != nil && b.hooks.OnStateChange != nil {
		b.hooks.OnStateChange(tr.from, tr.to, tr.reason)
	}
}

// Snapshot is a consistent view for admin/metrics.
type Snapshot struct {
	State               State
	OpenedAt            time.Time
	ConsecutiveFailures int
	Probes              int
	HalfOpenSuccesses   int
	WindowSuccesses     int64
	WindowFailures      int64
	WindowRequests      int64
	FailureRatio        float64
}

// Snapshot returns the current breaker state and rolling-window counts.
func (b *Breaker) Snapshot() Snapshot {
	now := b.clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	successes, failures := b.window.counts(now)
	ratio := 0.0
	if total := successes + failures; total > 0 {
		ratio = float64(failures) / float64(total)
	}
	return Snapshot{
		State:               State(b.state.Load()),
		OpenedAt:            b.openedAt,
		ConsecutiveFailures: b.consecutive,
		Probes:              b.probes,
		HalfOpenSuccesses:   b.successes,
		WindowSuccesses:     successes,
		WindowFailures:      failures,
		WindowRequests:      successes + failures,
		FailureRatio:        ratio,
	}
}

type transition struct {
	from, to State
	reason   string
}

type windowBucket struct {
	index   int64
	success int64
	failure int64
}

type rollingWindow struct {
	buckets   []windowBucket
	bucketDur time.Duration
}

func newRollingWindow(window time.Duration, buckets int) *rollingWindow {
	if window <= 0 {
		window = 10 * time.Second
	}
	if buckets < 1 {
		buckets = 10
	}
	return &rollingWindow{
		buckets:   make([]windowBucket, buckets),
		bucketDur: window / time.Duration(buckets),
	}
}

func (w *rollingWindow) record(now time.Time, success bool) {
	if w.bucketDur <= 0 {
		return
	}
	idx := now.UnixNano() / int64(w.bucketDur)
	slot := &w.buckets[int(idx%int64(len(w.buckets)))]
	if slot.index != idx {
		slot.index = idx
		slot.success = 0
		slot.failure = 0
	}
	if success {
		slot.success++
	} else {
		slot.failure++
	}
}

func (w *rollingWindow) counts(now time.Time) (successes, failures int64) {
	if w.bucketDur <= 0 {
		return 0, 0
	}
	current := now.UnixNano() / int64(w.bucketDur)
	for i := range w.buckets {
		b := &w.buckets[i]
		age := current - b.index
		if b.index == 0 || age < 0 || age >= int64(len(w.buckets)) {
			continue
		}
		successes += b.success
		failures += b.failure
	}
	return successes, failures
}

func (w *rollingWindow) reset() {
	for i := range w.buckets {
		w.buckets[i] = windowBucket{}
	}
}

// ClampRatio is exposed for callers converting config values.
func ClampRatio(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
