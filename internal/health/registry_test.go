package health

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

func newTestRegistry(t *testing.T, healthy, unhealthy int) (*Registry, *clock.Fake, *changeRecorder) {
	t.Helper()
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	rec := &changeRecorder{}
	reg := NewRegistry(healthy, unhealthy, c, rec.record)
	return reg, c, rec
}

type changeRecorder struct {
	mu     sync.Mutex
	events []Change
}

func (r *changeRecorder) record(c Change) {
	r.mu.Lock()
	r.events = append(r.events, c)
	r.mu.Unlock()
}

func (r *changeRecorder) all() []Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Change(nil), r.events...)
}

func TestRegisterAndSnapshot(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 2, 3)
	reg.Register("http://b")
	reg.Register("http://a")
	reg.Register("http://a")
	reg.Register("")

	if !reg.Available("http://a") || !reg.Available("http://unknown") {
		t.Fatal("targets should start available")
	}
	if got := reg.Targets(); len(got) != 2 || got[0] != "http://a" || got[1] != "http://b" {
		t.Fatalf("targets = %v", got)
	}
	snap := reg.Snapshot()
	if len(snap) != 2 || snap[0].Target != "http://a" || !snap[0].Available {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].LastChange.IsZero() {
		t.Fatal("registration should set last change")
	}
}

func TestFailureHysteresis(t *testing.T) {
	t.Parallel()
	reg, _, rec := newTestRegistry(t, 2, 3)
	reg.Register("http://a")

	reg.Failure("http://a", "boom")
	reg.Failure("http://a", "boom")
	if !reg.Available("http://a") {
		t.Fatal("ejected below threshold")
	}
	reg.Failure("http://a", "boom")
	if reg.Available("http://a") {
		t.Fatal("not ejected at threshold")
	}
	reg.Failure("http://a", "boom again")
	if reg.Available("http://a") {
		t.Fatal("unexpected recovery")
	}

	reg.Success("http://a")
	if reg.Available("http://a") {
		t.Fatal("recovered below healthy threshold")
	}
	reg.Success("http://a")
	if !reg.Available("http://a") {
		t.Fatal("did not recover at healthy threshold")
	}

	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Available || events[0].Reason != "unhealthy_threshold" {
		t.Fatalf("first event = %+v", events[0])
	}
	if !events[1].Available || events[1].Reason != "healthy_threshold" {
		t.Fatalf("second event = %+v", events[1])
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 2, 3)
	reg.Register("http://a")
	reg.Failure("http://a", "one")
	reg.Failure("http://a", "two")
	reg.Success("http://a")
	if got := reg.Snapshot()[0].ConsecutiveFailures; got != 0 {
		t.Fatalf("failures = %d", got)
	}
	reg.Failure("http://a", "three")
	reg.Failure("http://a", "four")
	if !reg.Available("http://a") {
		t.Fatal("success did not reset the failure streak")
	}
	reg.Failure("http://a", "five")
	if reg.Available("http://a") {
		t.Fatal("expected ejection")
	}
}

func TestFailureResetsRecoveryStreak(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 2, 1)
	reg.Register("http://a")
	reg.Failure("http://a", "down")
	if reg.Available("http://a") {
		t.Fatal("expected down")
	}
	reg.Success("http://a")
	reg.Failure("http://a", "still down")
	reg.Success("http://a")
	if reg.Available("http://a") {
		t.Fatal("failure should reset success streak")
	}
	reg.Success("http://a")
	if !reg.Available("http://a") {
		t.Fatal("expected recovery")
	}
}

func TestUnknownReportsIgnored(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 1, 1)
	reg.Failure("http://ghost", "boom")
	reg.Success("http://ghost")
	if got := reg.Targets(); len(got) != 0 {
		t.Fatalf("unknown target was inserted: %v", got)
	}
}

func TestSnapshotDetailsAndTruncation(t *testing.T) {
	t.Parallel()
	reg, c, _ := newTestRegistry(t, 1, 1)
	reg.Register("http://a")
	reg.Failure("http://a", strings.Repeat("x", 500))
	c.Advance(time.Second)
	reg.Success("http://a")
	snap := reg.Snapshot()[0]
	if !snap.Available || snap.ConsecutiveFailures != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	reg.Failure("http://a", strings.Repeat("y", 500))
	snap = reg.Snapshot()[0]
	if len(snap.LastError) != 200 {
		t.Fatalf("len(last error) = %d", len(snap.LastError))
	}
	if snap.LastProbe.IsZero() {
		t.Fatal("last probe not set")
	}
}

func TestThresholdDefaults(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(0, 0, nil, nil)
	reg.Register("http://a")
	reg.Failure("http://a", "x")
	reg.Failure("http://a", "x")
	if !reg.Available("http://a") {
		t.Fatal("default unhealthy threshold should be 3")
	}
	reg.Failure("http://a", "x")
	if reg.Available("http://a") {
		t.Fatal("expected ejection at default threshold")
	}
}

func TestConcurrentReports(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 5, 2)
	reg.Register("http://a")
	reg.Register("http://b")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := "http://a"
			if i%2 == 0 {
				target = "http://b"
			}
			for j := 0; j < 100; j++ {
				if j%3 == 0 {
					reg.Success(target)
				} else {
					reg.Failure(target, "load")
				}
				_ = reg.Available(target)
				_ = reg.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}
