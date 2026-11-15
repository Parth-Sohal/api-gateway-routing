package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/breaker"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

type captured struct {
	method  string
	path    string
	query   string
	host    string
	body    string
	headers http.Header
}

func TestForwardBasic(t *testing.T) {
	t.Parallel()
	seen := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- captured{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			host: r.Host, body: string(body), headers: r.Header.Clone(),
		}
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("X-Request-Id", "upstream-id")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "bad")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("world"))
	}))
	defer srv.Close()

	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", srv.URL+"/base")}}
	h := newHarness(t, cfg)
	route := testRoute("pool")
	route.StripPrefix = "/api"

	req, info := testRequest(http.MethodPost, "http://gateway.example/api/items?q=1", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("Connection", "X-Remove")
	req.Header.Set("X-Remove", "secret")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("Forwarded", "for=evil")
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, route)

	got := <-seen
	if got.method != http.MethodPost || got.path != "/base/items" || got.query != "q=1" {
		t.Fatalf("upstream request = %s %s?%s", got.method, got.path, got.query)
	}
	if got.host != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("host = %q", got.host)
	}
	if got.body != "hello" {
		t.Fatalf("body = %q", got.body)
	}
	for name, want := range map[string]string{
		"X-Forwarded-For":   "192.0.2.5",
		"X-Real-Ip":         "192.0.2.5",
		"X-Forwarded-Proto": "http",
		"X-Forwarded-Host":  "gateway.example",
		"X-Request-Id":      "req-1",
		"Authorization":     "Bearer abc",
	} {
		if got := got.headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"X-Remove", "Forwarded", "Connection"} {
		if v := got.headers.Get(name); v != "" {
			t.Errorf("hop-by-hop header %s leaked: %q", name, v)
		}
	}

	if rec.Code != http.StatusCreated || rec.Body.String() != "world" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("missing upstream header")
	}
	if rec.Header().Get(httpx.HeaderRequestID) != "req-1" {
		t.Fatalf("request id header = %q", rec.Header().Get(httpx.HeaderRequestID))
	}
	for _, name := range []string{"X-Hop", "Connection"} {
		if v := rec.Header().Get(name); v != "" {
			t.Fatalf("response hop header %s leaked: %q", name, v)
		}
	}
	if info.Upstream != "pool" || info.Retries != 0 || info.Target != srv.URL+"/base" {
		t.Fatalf("info = %+v", info)
	}

	body := renderMetrics(t, h.metrics)
	want := `gateway_upstream_requests_total{outcome="success",target="` + srv.URL + `/base",upstream="pool"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics missing %q\n%s", want, body)
	}
}

func TestPreserveHostAndStripPrefix(t *testing.T) {
	t.Parallel()
	type seenReq struct{ path, host string }
	seen := make(chan seenReq, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenReq{path: r.URL.Path, host: r.Host}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", srv.URL)}}
	h := newHarness(t, cfg)
	route := testRoute("pool")
	route.StripPrefix = "/api"
	route.PreserveHost = true

	req, _ := testRequest(http.MethodGet, "http://gateway.example/api/users", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, route)
	got := <-seen
	if got.path != "/users" || got.host != "gateway.example" {
		t.Fatalf("upstream saw %+v", got)
	}
}

func TestUnknownUpstreamAndNilRoute(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", "http://127.0.0.1:1")}}
	h := newHarness(t, cfg)

	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, &config.Route{Upstream: "missing"})
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != httpx.CodeInternalError {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}

	req, _ = testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec = httptest.NewRecorder()
	h.proxy.Forward(rec, req, nil)
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != httpx.CodeInternalError {
		t.Fatalf("nil route response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestNoHealthyTargets(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", srv.URL)}}
	h := newHarness(t, cfg)
	up, _ := h.proxy.Upstream("pool")
	target := up.Pool.Targets()[0].ID
	for i := 0; i < 3; i++ {
		up.Registry.Failure(target, "test")
	}

	req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != httpx.CodeNoHealthyTargets {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream was hit %d times", hits.Load())
	}
	if info.ErrorCode != httpx.CodeNoHealthyTargets {
		t.Fatalf("info = %+v", info)
	}
	if body := renderMetrics(t, h.metrics); !strings.Contains(body, `gateway_target_healthy{target="`+target+`",upstream="pool"} 0`) {
		t.Fatalf("health metric missing:\n%s", body)
	}
}

func TestBreakerRejectsAfterTrip(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("fail"))
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	enableBreaker(&u, 1, true, 10*time.Second)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "fail" {
		t.Fatalf("first response = %d %q", rec.Code, rec.Body.String())
	}
	up, _ := h.proxy.Upstream("pool")
	if up.Breaker.Snapshot().State != breaker.Open {
		t.Fatalf("breaker state = %v", up.Breaker.Snapshot().State)
	}

	req, _ = testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec = httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != httpx.CodeUpstreamUnavailable {
		t.Fatalf("second response = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on breaker rejection")
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}

	body := renderMetrics(t, h.metrics)
	for _, want := range []string{
		`gateway_breaker_state{upstream="pool"} 2`,
		`gateway_breaker_transitions_total{from="closed",reason="consecutive_failures",to="open",upstream="pool"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestBreakerIgnores500WhenDisabledForIt(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	enableBreaker(&u, 1, false, time.Second)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	for i := 0; i < 3; i++ {
		req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
		rec := httptest.NewRecorder()
		h.proxy.Forward(rec, req, testRoute("pool"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("response = %d", rec.Code)
		}
	}
	up, _ := h.proxy.Upstream("pool")
	if up.Breaker.Snapshot().State != breaker.Closed {
		t.Fatalf("500 tripped a breaker configured to ignore it: %v", up.Breaker.Snapshot().State)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	enableBreaker(&u, 1, true, time.Second)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	for i := 0; i < 3; i++ {
		req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
		rec := httptest.NewRecorder()
		h.proxy.Forward(rec, req, testRoute("pool"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("response = %d", rec.Code)
		}
	}
	up, _ := h.proxy.Upstream("pool")
	if up.Breaker.Snapshot().State != breaker.Closed {
		t.Fatalf("4xx tripped breaker: %v", up.Breaker.Snapshot().State)
	}
}

func TestConnectionErrorMapsTo502(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()

	u := testUpstream("pool", dead)
	u.HealthCheck.UnhealthyThreshold = 100
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusBadGateway || errorCode(t, rec) != httpx.CodeBadGateway {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if info.ErrorCode != httpx.CodeBadGateway {
		t.Fatalf("info = %+v", info)
	}
	up, _ := h.proxy.Upstream("pool")
	target := up.Pool.Targets()[0].ID
	if got := up.Registry.Snapshot()[0].ConsecutiveFailures; got != 1 {
		t.Fatalf("health failures = %d", got)
	}
	body := renderMetrics(t, h.metrics)
	if !strings.Contains(body, `gateway_upstream_requests_total{outcome="transport_error",target="`+target+`",upstream="pool"} 1`) {
		t.Fatalf("metrics missing transport error:\n%s", body)
	}
}

func TestTransportFailureEjectsTarget(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()

	u := testUpstream("pool", dead)
	u.HealthCheck.UnhealthyThreshold = 1
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first response = %d", rec.Code)
	}

	req, _ = testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec = httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != httpx.CodeNoHealthyTargets {
		t.Fatalf("second response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRouteTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})
	route := testRoute("pool")
	route.Timeout = config.Duration{Duration: 40 * time.Millisecond}

	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	begin := time.Now()
	h.proxy.Forward(rec, req, route)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("upstream was not contacted")
	}
	if rec.Code != http.StatusGatewayTimeout || errorCode(t, rec) != httpx.CodeGatewayTimeout {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if body := renderMetrics(t, h.metrics); !strings.Contains(body, `outcome="timeout"`) {
		t.Fatalf("timeout metric missing:\n%s", body)
	}
}

func TestStreamingResponse(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "first\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
		flusher.Flush()
	}))
	defer srv.Close()

	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", srv.URL)}}
	h := newHarness(t, cfg)
	route := testRoute("pool")
	route.Timeout = config.Duration{Duration: 5 * time.Second}

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.proxy.Forward(w, r, route)
	}))
	defer front.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(front.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, len("first\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if string(first) != "first\n" {
		t.Fatalf("first chunk = %q", first)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("rest: %v", err)
	}
	if string(rest) != "second\n" {
		t.Fatalf("rest = %q", rest)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	up, _ := h.proxy.Upstream("pool")
	if n := up.Pool.Snapshot()[0].Inflight; n != 0 {
		t.Fatalf("target still has %d in-flight requests", n)
	}
}

func TestHealthCheckWiring(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck = config.HealthCheck{
		Enabled:            true,
		Path:               "/healthz",
		Interval:           config.Duration{Duration: time.Hour},
		Timeout:            config.Duration{Duration: time.Second},
		HealthyThreshold:   1,
		UnhealthyThreshold: 1,
		ExpectedStatusMin:  200,
		ExpectedStatusMax:  299,
	}
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})
	up, _ := h.proxy.Upstream("pool")
	if up.Checker == nil {
		t.Fatal("checker not built")
	}
	target := up.Pool.Targets()[0].ID
	up.Checker.ProbeOnce(context.Background())
	if up.Registry.Available(target) {
		t.Fatal("failed probe did not eject target")
	}
	if body := renderMetrics(t, h.metrics); !strings.Contains(body, `gateway_target_probes_total{result="failure",target="`+target+`",upstream="pool"} 1`) {
		t.Fatalf("probe metric missing:\n%s", body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		h.proxy.RunHealthChecks(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunHealthChecks did not stop")
	}
}

func TestBuildErrors(t *testing.T) {
	t.Parallel()
	badHealth := testUpstream("pool", "http://127.0.0.1:1")
	badHealth.HealthCheck.Enabled = true
	badHealth.HealthCheck.Path = "healthz"

	cases := map[string]*config.Config{
		"nil":        nil,
		"empty name": {Upstreams: []config.Upstream{{Balancer: "round_robin", Targets: []config.Target{{URL: "http://a"}}}}},
		"duplicate":  {Upstreams: []config.Upstream{testUpstream("pool", "http://a"), testUpstream("pool", "http://b")}},
		"balancer":   {Upstreams: []config.Upstream{{Name: "pool", Balancer: "magic", Targets: []config.Target{{URL: "http://a"}}}}},
		"bad target": {Upstreams: []config.Upstream{testUpstream("pool", "ftp://a")}},
		"health":     {Upstreams: []config.Upstream{badHealth}},
	}
	for name, cfg := range cases {
		cfg := cfg
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Build(cfg, Deps{}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSnapshotsAndNames(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Upstreams: []config.Upstream{
		testUpstream("z", "http://127.0.0.1:1"),
		testUpstream("a", "http://127.0.0.1:2"),
	}}
	h := newHarness(t, cfg)
	if names := h.proxy.Names(); len(names) != 2 || names[0] != "a" || names[1] != "z" {
		t.Fatalf("names = %v", names)
	}
	snaps := h.proxy.Snapshots()
	if len(snaps) != 2 || snaps[0].Name != "a" || len(snaps[0].Targets) != 1 || len(snaps[0].Health) != 1 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	if snaps[0].Breaker != nil {
		t.Fatal("disabled breaker should have no snapshot")
	}
	if _, ok := h.proxy.Upstream("missing"); ok {
		t.Fatal("unexpected upstream")
	}
}
