package main

import (
	"sync"
	"time"
)

// Permit records the circuit breaker's decision for one request.
type Permit struct {
	id         uint64
	generation uint64
	isProbe    bool
}

// State represents the circuit breaker's current state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	}
	return "UNKNOWN"
}

// CircuitBreaker protects an upstream from being hammered while it's failing.
type CircuitBreaker struct {
	mu sync.Mutex

	state            State
	failureThreshold int
	successThreshold int
	cooldownPeriod   time.Duration

	consecutiveFailures int // 0
	halfOpenSuccesses   int // 0
	halfOpenInFlight    int // 0

	lastStateChange time.Time

	generation    uint64
	nextPermitID  uint64
	activePermits map[uint64]Permit
}

// NewCircuitBreaker creates a breaker with the given thresholds.
func NewCircuitBreaker(
	failureThreshold int,
	successThreshold int,
	cooldown time.Duration,
) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		cooldownPeriod:   cooldown,
		lastStateChange:  time.Now(),
		activePermits:    make(map[uint64]Permit),
	}
}

func (cb *CircuitBreaker) newPermit(isProbe bool) *Permit {
	cb.nextPermitID++

	permit := &Permit{
		id:         cb.nextPermitID,
		generation: cb.generation,
		isProbe:    isProbe,
	}

	cb.activePermits[permit.id] = *permit

	return permit
}

// Allow reports whether the circuit breaker permits a request right now.
func (cb *CircuitBreaker) Allow() (*Permit, bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen {
		if time.Since(cb.lastStateChange) < cb.cooldownPeriod {
			return nil, false
		}

		cb.state = StateHalfOpen
		cb.generation++
		cb.lastStateChange = time.Now()
		cb.halfOpenSuccesses = 0
		cb.halfOpenInFlight = 0
	}

	switch cb.state {
	case StateClosed:
		return cb.newPermit(false), true

	case StateHalfOpen:
		if cb.halfOpenInFlight >= cb.successThreshold {
			return nil, false
		}

		cb.halfOpenInFlight++
		return cb.newPermit(true), true

	default:
		return nil, false
	}
}

// consume permit
func (cb *CircuitBreaker) consumePermit(permit *Permit) bool {
	if permit == nil {
		return false
	}

	active, exists := cb.activePermits[permit.id]
	if !exists {
		return false
	}

	// Verify that the supplied permit matches the registered one.
	if active.generation != permit.generation ||
		active.isProbe != permit.isProbe {
		return false
	}

	delete(cb.activePermits, permit.id)
	return true
}

// RecordSuccess should be called after a successful upstream request.
func (cb *CircuitBreaker) RecordSuccess(permit *Permit) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if !cb.consumePermit(permit) {
		return
	}

	// Ignore results from an older generation.
	if permit.generation != cb.generation {
		return
	}

	if permit.isProbe {
		if cb.state != StateHalfOpen {
			return
		}

		cb.halfOpenInFlight--
		cb.halfOpenSuccesses++

		if cb.halfOpenSuccesses >= cb.successThreshold {
			cb.state = StateClosed
			cb.generation++
			cb.consecutiveFailures = 0
			cb.halfOpenSuccesses = 0
			cb.halfOpenInFlight = 0
			cb.lastStateChange = time.Now()
		}

		return
	}

	// Ordinary request.
	if cb.state == StateClosed {
		cb.consecutiveFailures = 0
	}
}

// RecordFailure should be called after a failed upstream request.
func (cb *CircuitBreaker) RecordFailure(permit *Permit) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if !cb.consumePermit(permit) {
		return
	}

	// Ignore results from an older generation.
	if permit.generation != cb.generation {
		return
	}

	if permit.isProbe {
		if cb.state != StateHalfOpen {
			return
		}

		cb.halfOpenInFlight--

		// A failed recovery probe means the service
		// is still unhealthy.
		cb.state = StateOpen
		cb.generation++
		cb.halfOpenSuccesses = 0
		cb.halfOpenInFlight = 0
		cb.lastStateChange = time.Now()

		return
	}

	// Ordinary request.
	if cb.state != StateClosed {
		return
	}

	cb.consecutiveFailures++

	if cb.consecutiveFailures >= cb.failureThreshold {
		cb.state = StateOpen
		cb.generation++
		cb.consecutiveFailures = 0
		cb.halfOpenSuccesses = 0
		cb.halfOpenInFlight = 0
		cb.lastStateChange = time.Now()
	}
}

// CurrentState returns the state (useful for logging and tests).
func (cb *CircuitBreaker) CurrentState() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}
