package breaker

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

var errUpstream = errors.New("upstream failed")

func newTestBreaker(t *testing.T, cfg Config) (*Breaker, *clock.Fake, *[]string) {
	t.Helper()
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	var events []string
	var mu sync.Mutex
	b := New(cfg, c, Hooks{
		OnStateChange: func(from, to State, reason string) {
			mu.Lock()
			events = append(events, fmt.Sprintf("%s->%s:%s", from, to, reason))
			mu.Unlock()
		},
		OnRejected: func(reason string) {
			mu.Lock()
			events = append(events, "rejected:"+reason)
			mu.Unlock()
		},
	})
	return b, c, &events
}

func mustAllow(t *testing.T, b *Breaker) func(error) {
	t.Helper()
	done, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if done == nil {
		t.Fatal("nil completion callback")
	}
	return done
}

func fail(t *testing.T, b *Breaker) {
	t.Helper()
	mustAllow(t, b)(errUpstream)
}

func TestTripsOnConsecutiveFailures(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 2, FailureRatio: 1, MinRequests: 100,
		Window: 10 * time.Second, OpenTimeout: time.Second,
	})
	if b.State() != Closed {
		t.Fatal("initial state")
	}
	mustAllow(t, b)(nil)
	if b.State() != Closed {
		t.Fatal("success should not trip")
	}
	fail(t, b)
	if b.State() != Closed {
		t.Fatal("one failure should not trip")
	}
	fail(t, b)
	if b.State() != Open {
		t.Fatalf("state = %v", b.State())
	}
	snap := b.Snapshot()
	if snap.WindowFailures != 2 || snap.WindowSuccesses != 1 || snap.WindowRequests != 3 ||
		snap.ConsecutiveFailures != 2 || snap.FailureRatio < 0.66 || snap.FailureRatio > 0.67 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestOpenRejectsAndHalfOpen(t *testing.T) {
	b, c, _ := newTestBreaker(t, Config{
		FailureThreshold: 1, FailureRatio: 1, MinRequests: 100,
		Window: 10 * time.Second, OpenTimeout: 5 * time.Second,
		HalfOpenMaxProbes: 2, HalfOpenSuccesses: 2,
	})
	fail(t, b)
	if b.State() != Open {
		t.Fatal("should be open")
	}
	_, err := b.Allow()
	var oe *OpenError
	if !errors.As(err, &oe) || oe.RetryAfter <= 0 || oe.RetryAfter > 5*time.Second {
		t.Fatalf("error = %#v", err)
	}
	c.Advance(2 * time.Second)
	_, err = b.Allow()
	if !errors.As(err, &oe) || oe.RetryAfter > 3*time.Second {
		t.Fatalf("error after 2s = %#v", err)
	}

	c.Advance(3 * time.Second)
	done1 := mustAllow(t, b)
	if b.State() != HalfOpen {
		t.Fatalf("state = %v", b.State())
	}
	done2 := mustAllow(t, b)
	if _, err := b.Allow(); !errors.As(err, &oe) || oe.Reason != "half_open_probes_exhausted" {
		t.Fatalf("third probe error = %v", err)
	}
	done1(nil)
	if b.State() != HalfOpen {
		t.Fatal("one success should not close")
	}
	done2(nil)
	if b.State() != Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b, c, _ := newTestBreaker(t, Config{
		FailureThreshold: 1, FailureRatio: 1, MinRequests: 100,
		Window: 10 * time.Second, OpenTimeout: time.Second,
		HalfOpenMaxProbes: 1, HalfOpenSuccesses: 1,
	})
	fail(t, b)
	c.Advance(2 * time.Second)
	done := mustAllow(t, b)
	if b.State() != HalfOpen {
		t.Fatal("expected half-open")
	}
	done(errUpstream)
	if b.State() != Open {
		t.Fatalf("state = %v", b.State())
	}
	if _, err := b.Allow(); err == nil {
		t.Fatal("expected rejection after probe failure")
	}
}

func TestHalfOpenIgnoredReleasesProbe(t *testing.T) {
	b, c, _ := newTestBreaker(t, Config{
		FailureThreshold: 1, FailureRatio: 1, MinRequests: 100,
		Window: 10 * time.Second, OpenTimeout: time.Second,
		HalfOpenMaxProbes: 1, HalfOpenSuccesses: 1,
	})
	fail(t, b)
	c.Advance(2 * time.Second)
	done := mustAllow(t, b)
	done(ErrIgnored)
	if b.State() != HalfOpen {
		t.Fatalf("ignored probe changed state to %v", b.State())
	}
	done2 := mustAllow(t, b)
	done2(nil)
	if b.State() != Closed {
		t.Fatalf("state = %v", b.State())
	}
}

func TestIgnoredOutcomesDoNotCount(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 2, FailureRatio: 1, MinRequests: 100, Window: 10 * time.Second,
	})
	mustAllow(t, b)(ErrIgnored)
	mustAllow(t, b)(ErrIgnored)
	if b.State() != Closed {
		t.Fatal("ignored outcomes should not trip")
	}
	fail(t, b)
	fail(t, b)
	if b.State() != Open {
		t.Fatal("real failures should still trip")
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 2, FailureRatio: 1, MinRequests: 100, Window: 10 * time.Second,
	})
	fail(t, b)
	mustAllow(t, b)(nil)
	fail(t, b)
	if b.State() != Closed {
		t.Fatal("success should reset consecutive count")
	}
	fail(t, b)
	if b.State() != Open {
		t.Fatal("expected open")
	}
}

func TestFailureRatio(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 100, FailureRatio: 0.5, MinRequests: 4,
		Window: 10 * time.Second, OpenTimeout: time.Second,
	})
	mustAllow(t, b)(nil)
	mustAllow(t, b)(nil)
	fail(t, b)
	if b.State() != Closed {
		t.Fatal("below min requests")
	}
	fail(t, b)
	if b.State() != Open {
		t.Fatalf("state = %v", b.State())
	}
}

func TestRatioWindowExpiry(t *testing.T) {
	b, c, _ := newTestBreaker(t, Config{
		FailureThreshold: 100, FailureRatio: 0.5, MinRequests: 4,
		Window: 10 * time.Second, Buckets: 10, OpenTimeout: time.Second,
	})
	fail(t, b)
	fail(t, b)
	c.Advance(11 * time.Second)
	mustAllow(t, b)(nil)
	mustAllow(t, b)(nil)
	fail(t, b)
	if b.State() != Closed {
		t.Fatalf("old failures counted: state = %v", b.State())
	}
}

func TestDoneCallbackCalledOnce(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 2, FailureRatio: 1, MinRequests: 100, Window: 10 * time.Second,
	})
	done := mustAllow(t, b)
	done(errUpstream)
	done(errUpstream)
	if b.State() != Closed {
		t.Fatal("double completion should be ignored")
	}
	fail(t, b)
	if b.State() != Open {
		t.Fatal("expected open after second distinct failure")
	}
}

func TestHooksAndRetryAfter(t *testing.T) {
	b, c, events := newTestBreaker(t, Config{
		FailureThreshold: 1, FailureRatio: 1, MinRequests: 100,
		Window: 10 * time.Second, OpenTimeout: 2 * time.Second,
		HalfOpenMaxProbes: 1, HalfOpenSuccesses: 1,
	})
	fail(t, b)
	if _, err := b.Allow(); err == nil {
		t.Fatal("expected rejection")
	}
	c.Advance(3 * time.Second)
	done := mustAllow(t, b)
	done(nil)
	if b.State() != Closed {
		t.Fatal("expected closed")
	}
	want := []string{
		"closed->open:consecutive_failures",
		"rejected:open",
		"open->half_open:open_timeout",
		"half_open->closed:probe_success",
	}
	if len(*events) != len(want) {
		t.Fatalf("events = %v", *events)
	}
	for i := range want {
		if (*events)[i] != want[i] {
			t.Fatalf("events = %v, want %v", *events, want)
		}
	}
}

func TestDefaultsAndClamp(t *testing.T) {
	b := New(Config{FailureRatio: 5}, nil, Hooks{})
	if b.cfg.FailureRatio != 1 {
		t.Fatalf("ratio = %v", b.cfg.FailureRatio)
	}
	if b.cfg.FailureThreshold != 5 || b.cfg.OpenTimeout != 5*time.Second {
		t.Fatalf("defaults = %+v", b.cfg)
	}
	if ClampRatio(-1) != 0 || ClampRatio(2) != 1 || ClampRatio(0.25) != 0.25 {
		t.Fatal("ClampRatio")
	}
	if Closed.String() != "closed" || HalfOpen.String() != "half_open" || Open.String() != "open" {
		t.Fatal("state strings")
	}
	if State(99).String() != "unknown" {
		t.Fatal("unknown state string")
	}
}

func TestConcurrentUse(t *testing.T) {
	b, _, _ := newTestBreaker(t, Config{
		FailureThreshold: 10, FailureRatio: 0.9, MinRequests: 50,
		Window: 10 * time.Second, OpenTimeout: time.Second,
	})
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			done, err := b.Allow()
			if err != nil {
				return
			}
			if i%3 == 0 {
				done(nil)
			} else {
				done(errUpstream)
			}
		}(i)
	}
	wg.Wait()
	if s := b.State(); s != Open && s != Closed {
		t.Fatalf("unexpected state %v", s)
	}
	_ = b.Snapshot()
}
