package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func newCheckerFor(t *testing.T, srv *httptest.Server, hc config.HealthCheck, reg *Registry, onProbe func(ProbeResult)) *Checker {
	t.Helper()
	u := mustURL(t, srv.URL)
	c, err := NewChecker(hc, []Target{{ID: u.String(), URL: u}}, reg, srv.Client(), nil, onProbe)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return c
}

func TestProbeOnceHealthTransitions(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	var mu sync.Mutex
	var probes []ProbeResult
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout: config.Duration{Duration: time.Second},
	}, reg, func(p ProbeResult) {
		mu.Lock()
		probes = append(probes, p)
		mu.Unlock()
	})

	c.ProbeOnce(context.Background())
	target := reg.Targets()[0]
	if !reg.Available(target) {
		t.Fatal("healthy probe should keep target available")
	}

	status.Store(http.StatusServiceUnavailable)
	c.ProbeOnce(context.Background())
	if reg.Available(target) {
		t.Fatal("503 should eject target")
	}
	snap := reg.Snapshot()[0]
	if !strings.Contains(snap.LastError, "503") {
		t.Fatalf("last error = %q", snap.LastError)
	}

	status.Store(http.StatusOK)
	c.ProbeOnce(context.Background())
	if !reg.Available(target) {
		t.Fatal("healthy probe should recover target")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(probes) != 3 {
		t.Fatalf("probes = %+v", probes)
	}
	if !probes[0].Healthy || probes[1].Healthy || !probes[2].Healthy {
		t.Fatalf("probe results = %+v", probes)
	}
	if probes[0].Duration <= 0 || probes[0].StatusCode != http.StatusOK {
		t.Fatalf("probe result = %+v", probes[0])
	}
}

func TestProbeStatusRange(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout:           config.Duration{Duration: time.Second},
		ExpectedStatusMin: 200, ExpectedStatusMax: 204,
	}, reg, nil)
	c.ProbeOnce(context.Background())
	if !reg.Available(reg.Targets()[0]) {
		t.Fatal("204 should be healthy")
	}
	status.Store(http.StatusPartialContent)
	c.ProbeOnce(context.Background())
	if reg.Available(reg.Targets()[0]) {
		t.Fatal("206 should be outside 200-204")
	}
}

func TestProbePathJoin(t *testing.T) {
	t.Parallel()
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := mustURL(t, srv.URL+"/base/")
	reg, _, _ := newTestRegistry(t, 1, 1)
	c, err := NewChecker(config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout: config.Duration{Duration: time.Second},
	}, []Target{{ID: "base", URL: base}}, reg, srv.Client(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ProbeOnce(context.Background())
	if got := gotPath.Load(); got != "/base/healthz" {
		t.Fatalf("probe path = %v", got)
	}
}

func TestProbeTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout: config.Duration{Duration: 30 * time.Millisecond},
	}, reg, nil)
	c.ProbeOnce(context.Background())
	select {
	case <-started:
	default:
		t.Fatal("probe did not reach server")
	}
	if reg.Available(reg.Targets()[0]) {
		t.Fatal("timed-out probe should eject")
	}
}

func TestProbeCancellationIsNotAFailure(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout: config.Duration{Duration: 5 * time.Second},
	}, reg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.ProbeOnce(ctx)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ProbeOnce did not return after cancellation")
	}
	snap := reg.Snapshot()[0]
	if !snap.Available || snap.ConsecutiveFailures != 0 || !snap.LastProbe.IsZero() {
		t.Fatalf("cancellation affected health: %+v", snap)
	}
}

func TestRunCancellation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	first := make(chan struct{})
	var once sync.Once
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled:  true,
		Path:     "/healthz",
		Interval: config.Duration{Duration: 5 * time.Millisecond},
		Timeout:  config.Duration{Duration: time.Second},
	}, reg, func(ProbeResult) {
		once.Do(func() { close(first) })
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not probe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	c.CloseIdleConnections()
	if calls.Load() == 0 {
		t.Fatal("expected at least one probe")
	}
}

func TestRunDisabledReturns(t *testing.T) {
	t.Parallel()
	reg, _, _ := newTestRegistry(t, 1, 1)
	c, err := NewChecker(config.HealthCheck{Enabled: false, Path: "/healthz"},
		[]Target{{ID: "http://a", URL: mustURL(t, "http://a")}}, reg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled Run blocked")
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			followed.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/ok", http.StatusFound)
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	c := newCheckerFor(t, srv, config.HealthCheck{
		Enabled: true, Path: "/healthz",
		Timeout: config.Duration{Duration: time.Second},
	}, reg, nil)
	c.ProbeOnce(context.Background())
	if followed.Load() {
		t.Fatal("probe followed a redirect")
	}
	if !reg.Available(reg.Targets()[0]) {
		t.Fatal("302 is inside the default 200-399 range")
	}
}

func TestNewCheckerValidation(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(1, 1, nil, nil)
	okTarget := Target{ID: "http://a", URL: mustURL(t, "http://a")}
	withQuery := mustURL(t, "http://a?x=1")
	cases := []struct {
		name    string
		cfg     config.HealthCheck
		targets []Target
		reg     *Registry
	}{
		{"nil registry", config.HealthCheck{Path: "/h"}, []Target{okTarget}, nil},
		{"bad path", config.HealthCheck{Path: "h"}, []Target{okTarget}, reg},
		{"bad status min", config.HealthCheck{Path: "/h", ExpectedStatusMin: 99}, []Target{okTarget}, reg},
		{"status inverted", config.HealthCheck{Path: "/h", ExpectedStatusMin: 500, ExpectedStatusMax: 400}, []Target{okTarget}, reg},
		{"empty id", config.HealthCheck{Path: "/h"}, []Target{{URL: mustURL(t, "http://a")}}, reg},
		{"nil url", config.HealthCheck{Path: "/h"}, []Target{{ID: "a"}}, reg},
		{"bad scheme", config.HealthCheck{Path: "/h"}, []Target{{ID: "a", URL: mustURL(t, "ftp://a")}}, reg},
		{"userinfo", config.HealthCheck{Path: "/h"}, []Target{{ID: "a", URL: mustURL(t, "http://user:pass@a")}}, reg},
		{"query", config.HealthCheck{Path: "/h"}, []Target{{ID: "a", URL: withQuery}}, reg},
		{"duplicate", config.HealthCheck{Path: "/h"}, []Target{okTarget, okTarget}, reg},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewChecker(tc.cfg, tc.targets, tc.reg, nil, nil, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCheckerDefaults(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg, _, _ := newTestRegistry(t, 1, 1)
	c := newCheckerFor(t, srv, config.HealthCheck{Enabled: true}, reg, nil)
	c.ProbeOnce(context.Background())
	if !reg.Available(reg.Targets()[0]) {
		t.Fatal("default probe failed")
	}
}
