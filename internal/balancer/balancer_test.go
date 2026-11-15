package balancer

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func targets(urls ...string) []config.Target {
	out := make([]config.Target, len(urls))
	for i, u := range urls {
		out[i] = config.Target{URL: u, Weight: 1}
	}
	return out
}

func newPool(t *testing.T, strategy string, h Health, urls ...string) *Pool {
	t.Helper()
	p, err := New("api", strategy, targets(urls...), h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func mustAcquire(t *testing.T, p *Pool, exclude map[string]struct{}) *Target {
	t.Helper()
	tgt, err := p.Acquire(exclude)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return tgt
}

type fakeHealth struct {
	mu   sync.RWMutex
	down map[string]bool
}

func (f *fakeHealth) Available(id string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return !f.down[id]
}

func (f *fakeHealth) setDown(id string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down == nil {
		f.down = make(map[string]bool)
	}
	f.down[id] = down
}

func TestParseStrategy(t *testing.T) {
	t.Parallel()
	cases := map[string]Strategy{
		"":                     RoundRobin,
		"round_robin":          RoundRobin,
		"round-robin":          RoundRobin,
		"rr":                   RoundRobin,
		"weighted":             WeightedRoundRobin,
		"weighted_round_robin": WeightedRoundRobin,
		"weighted-round-robin": WeightedRoundRobin,
		"wrr":                  WeightedRoundRobin,
		"least_conn":           LeastConn,
		"least-connections":    LeastConn,
		"least_connections":    LeastConn,
		"leastconn":            LeastConn,
	}
	for in, want := range cases {
		got, err := ParseStrategy(in)
		if err != nil || got != want {
			t.Errorf("ParseStrategy(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseStrategy("magic"); err == nil {
		t.Error("expected error for unknown strategy")
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		strategy string
		targets  []config.Target
	}{
		{"unknown strategy", "magic", targets("http://a")},
		{"no targets", "round_robin", nil},
		{"bad url", "round_robin", targets("://bad")},
		{"bad scheme", "round_robin", targets("ftp://a")},
		{"no host", "round_robin", targets("http:///path")},
		{"duplicate", "round_robin", targets("http://a", "http://a")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New("api", tc.strategy, tc.targets, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestZeroWeightDefaultsToOne(t *testing.T) {
	t.Parallel()
	p, err := New("api", "weighted_round_robin", []config.Target{{URL: "http://a"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Targets()[0].Weight != 1 {
		t.Fatalf("weight = %d", p.Targets()[0].Weight)
	}
}

func TestRoundRobinCycles(t *testing.T) {
	t.Parallel()
	p := newPool(t, "round_robin", nil, "http://a", "http://b", "http://c")
	var got []string
	for i := 0; i < 6; i++ {
		tgt := mustAcquire(t, p, nil)
		got = append(got, tgt.ID)
		p.Release(tgt)
	}
	want := []string{"http://a", "http://b", "http://c", "http://a", "http://b", "http://c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence = %v, want %v", got, want)
		}
	}
}

func TestRoundRobinSkipsExcluded(t *testing.T) {
	t.Parallel()
	p := newPool(t, "round_robin", nil, "http://a", "http://b", "http://c")
	exclude := map[string]struct{}{"http://a": {}}
	for i := 0; i < 6; i++ {
		tgt := mustAcquire(t, p, exclude)
		if tgt.ID == "http://a" {
			t.Fatal("selected excluded target")
		}
		p.Release(tgt)
	}
	if _, err := p.Acquire(map[string]struct{}{
		"http://a": {}, "http://b": {}, "http://c": {},
	}); !errors.Is(err, ErrNoTargets) {
		t.Fatalf("error = %v", err)
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	t.Parallel()
	h := &fakeHealth{}
	p := newPool(t, "round_robin", h, "http://a", "http://b", "http://c")
	h.setDown("http://a", true)
	h.setDown("http://c", true)
	for i := 0; i < 4; i++ {
		tgt := mustAcquire(t, p, nil)
		if tgt.ID != "http://b" {
			t.Fatalf("selected %s", tgt.ID)
		}
		p.Release(tgt)
	}
	h.setDown("http://b", true)
	if _, err := p.Acquire(nil); !errors.Is(err, ErrNoTargets) {
		t.Fatalf("error = %v", err)
	}
	if got := p.Snapshot(); len(got) != 3 || got[1].Available {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestWeightedDistribution(t *testing.T) {
	t.Parallel()
	p, err := New("api", "weighted_round_robin", []config.Target{
		{URL: "http://a", Weight: 5},
		{URL: "http://b", Weight: 1},
		{URL: "http://c", Weight: 1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	const picks = 700
	for i := 0; i < picks; i++ {
		tgt := mustAcquire(t, p, nil)
		counts[tgt.ID]++
		p.Release(tgt)
	}
	if counts["http://a"] != 500 || counts["http://b"] != 100 || counts["http://c"] != 100 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestWeightedSkipsUnhealthyAndResetsCredit(t *testing.T) {
	t.Parallel()
	h := &fakeHealth{}
	p, err := New("api", "weighted_round_robin", []config.Target{
		{URL: "http://a", Weight: 3},
		{URL: "http://b", Weight: 1},
	}, h)
	if err != nil {
		t.Fatal(err)
	}
	h.setDown("http://a", true)
	for i := 0; i < 5; i++ {
		tgt := mustAcquire(t, p, nil)
		if tgt.ID != "http://b" {
			t.Fatalf("selected %s while a is down", tgt.ID)
		}
		p.Release(tgt)
	}
	h.setDown("http://a", false)
	// The recovered heavy target must not monopolize more than its share.
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		tgt := mustAcquire(t, p, nil)
		counts[tgt.ID]++
		p.Release(tgt)
	}
	if counts["http://a"] != 3 || counts["http://b"] != 1 {
		t.Fatalf("counts after recovery = %v", counts)
	}
}

func TestLeastConn(t *testing.T) {
	t.Parallel()
	p := newPool(t, "least_conn", nil, "http://a", "http://b", "http://c")
	a := mustAcquire(t, p, nil)
	b := mustAcquire(t, p, nil)
	c := mustAcquire(t, p, nil)
	if a.ID == b.ID || b.ID == c.ID || a.ID == c.ID {
		t.Fatalf("least_conn reused targets: %s %s %s", a.ID, b.ID, c.ID)
	}
	if a.Inflight() != 1 || b.Inflight() != 1 || c.Inflight() != 1 {
		t.Fatalf("inflight = %d %d %d", a.Inflight(), b.Inflight(), c.Inflight())
	}
	p.Release(b)
	next := mustAcquire(t, p, nil)
	if next.ID != b.ID {
		t.Fatalf("selected %s, want released %s", next.ID, b.ID)
	}
	p.Release(a)
	p.Release(b)
	p.Release(c)
	p.Release(next)
	for _, tgt := range p.Targets() {
		if tgt.Inflight() != 0 {
			t.Fatalf("inflight leak on %s: %d", tgt.ID, tgt.Inflight())
		}
	}
}

func TestLeastConnTieRotates(t *testing.T) {
	t.Parallel()
	p := newPool(t, "least_conn", nil, "http://a", "http://b", "http://c")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		tgt := mustAcquire(t, p, nil)
		seen[tgt.ID] = true
		p.Release(tgt)
	}
	if len(seen) != 3 {
		t.Fatalf("tie breaker did not rotate: %v", seen)
	}
}

func TestLeastConnRespectsHealthAndExclude(t *testing.T) {
	t.Parallel()
	h := &fakeHealth{}
	p := newPool(t, "least_conn", h, "http://a", "http://b", "http://c")
	h.setDown("http://a", true)
	tgt := mustAcquire(t, p, map[string]struct{}{"http://b": {}})
	if tgt.ID != "http://c" {
		t.Fatalf("selected %s", tgt.ID)
	}
	p.Release(tgt)
}

func TestReleaseClampsAndNil(t *testing.T) {
	t.Parallel()
	p := newPool(t, "round_robin", nil, "http://a")
	p.Release(nil)
	tgt := p.Targets()[0]
	p.Release(tgt)
	p.Release(tgt)
	if tgt.Inflight() != 0 {
		t.Fatalf("inflight = %d", tgt.Inflight())
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	p := newPool(t, "round_robin", nil, "http://a", "http://b")
	tgt := mustAcquire(t, p, nil)
	defer p.Release(tgt)
	snap := p.Snapshot()
	if len(snap) != 2 || !snap[0].Available || !snap[1].Available {
		t.Fatalf("snapshot = %+v", snap)
	}
	var total int64
	for _, s := range snap {
		if s.ID == "" || s.Weight != 1 {
			t.Fatalf("snapshot = %+v", snap)
		}
		total += s.Inflight
	}
	if total != 1 {
		t.Fatalf("total inflight = %d", total)
	}
	if p.Name() != "api" || len(p.Targets()) != 2 || p.Strategy() != RoundRobin {
		t.Fatal("accessors")
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	t.Parallel()
	p := newPool(t, "round_robin", nil, "http://a", "http://b", "http://c")
	var failures atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tgt, err := p.Acquire(nil)
				if err != nil {
					failures.Add(1)
					return
				}
				_ = tgt.Inflight()
				p.Release(tgt)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("acquire failures = %d", failures.Load())
	}
	for _, tgt := range p.Targets() {
		if got := tgt.Inflight(); got != 0 {
			t.Fatalf("inflight leak on %s: %d", tgt.ID, got)
		}
	}
}
