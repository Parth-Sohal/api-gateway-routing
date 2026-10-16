package main

import (
	"testing"
	"time"
)

func TestStartsClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 100*time.Millisecond)
	if cb.CurrentState() != StateClosed {
		t.Fatalf("expected CLOSED, got %v", cb.CurrentState())
	}
}

func TestTripsAfterFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 100*time.Millisecond)

	for i := 1; i <= 3; i++ {
		permit, ok := cb.Allow()
		if !ok {
			t.Fatalf("request %d should be allowed (still CLOSED)", i)
		}
		cb.RecordFailure(permit)
	}

	if cb.CurrentState() != StateOpen {
		t.Fatalf("expected OPEN, got %v", cb.CurrentState())
	}

	if _, ok := cb.Allow(); ok {
		t.Fatal("expected request to be denied (OPEN)")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 100*time.Millisecond)

	p1, _ := cb.Allow()
	cb.RecordFailure(p1)
	p2, _ := cb.Allow()
	cb.RecordFailure(p2)

	p3, _ := cb.Allow()
	cb.RecordSuccess(p3) // resets

	p4, _ := cb.Allow()
	cb.RecordFailure(p4)
	p5, _ := cb.Allow()
	cb.RecordFailure(p5)

	if cb.CurrentState() != StateClosed {
		t.Fatalf("expected CLOSED, got %v", cb.CurrentState())
	}
}

func TestCooldownTransition(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 100*time.Millisecond)

	p1, _ := cb.Allow()
	cb.RecordFailure(p1)
	p2, _ := cb.Allow()
	cb.RecordFailure(p2)

	if cb.CurrentState() != StateOpen {
		t.Fatalf("expected OPEN, got %v", cb.CurrentState())
	}

	if _, ok := cb.Allow(); ok {
		t.Fatal("should be denied during cooldown")
	}

	time.Sleep(150 * time.Millisecond)

	_, ok := cb.Allow()
	if !ok {
		t.Fatal("should be allowed after cooldown (probe)")
	}

	if cb.CurrentState() != StateHalfOpen {
		t.Fatalf("expected HALF-OPEN, got %v", cb.CurrentState())
	}
}

func TestHalfOpenClosesOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 50*time.Millisecond)

	p1, _ := cb.Allow()
	cb.RecordFailure(p1)
	p2, _ := cb.Allow()
	cb.RecordFailure(p2)

	time.Sleep(60 * time.Millisecond)

	// Two successful probes
	p3, _ := cb.Allow()
	cb.RecordSuccess(p3)
	p4, _ := cb.Allow()
	cb.RecordSuccess(p4)

	if cb.CurrentState() != StateClosed {
		t.Fatalf("expected CLOSED, got %v", cb.CurrentState())
	}
}

func TestHalfOpenReopensOnFailure(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 50*time.Millisecond)

	p1, _ := cb.Allow()
	cb.RecordFailure(p1)
	p2, _ := cb.Allow()
	cb.RecordFailure(p2)

	time.Sleep(60 * time.Millisecond)

	p3, _ := cb.Allow()
	cb.RecordFailure(p3)

	if cb.CurrentState() != StateOpen {
		t.Fatalf("expected OPEN, got %v", cb.CurrentState())
	}
}
