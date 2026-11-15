package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/balancer"
	"github.com/Parth-Sohal/api-gateway-routing/internal/breaker"
	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/health"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/proxy"
)

const testToken = "s3cret-admin-token"

type fakeSource struct {
	mu         sync.Mutex
	snapshot   Snapshot
	ready      bool
	reason     string
	reloadErr  error
	reloadCall int
}

func (f *fakeSource) AdminSnapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeSource) Reload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadCall++
	if f.reloadErr != nil {
		return f.reloadErr
	}
	f.snapshot.Reloads++
	f.snapshot.LastReload = time.Unix(1_700_000_100, 0)
	f.snapshot.LastReloadError = ""
	return nil
}

func (f *fakeSource) Ready() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready, f.reason
}

func snapshotFixture() Snapshot {
	changed := time.Unix(1_700_000_050, 0).UTC()
	cfg := &config.Config{
		Redis: config.Redis{},
		Upstreams: []config.Upstream{{
			Name:     "api",
			Balancer: "least_conn",
		}},
		Routes: []config.Route{{
			Name:         "api",
			Match:        config.Match{Host: "api.example", PathPrefix: "/v1", Methods: []string{"GET", "POST"}},
			Upstream:     "api",
			Auth:         config.RouteAuth{Required: true, Scopes: []string{"read"}},
			RateLimit:    &config.RateLimit{Capacity: 40, RefillPerSec: 20, Key: "ip"},
			Timeout:      config.Duration{Duration: 5 * time.Second},
			MaxBodyBytes: 4096,
		}},
	}
	return Snapshot{
		Config:      cfg,
		RateLimiter: "redis",
		Reloads:     2,
		Upstreams: []proxy.UpstreamSnapshot{{
			Name: "api",
			Targets: []balancer.TargetSnapshot{
				{ID: "http://10.0.0.2", Weight: 2, Inflight: 3, Available: true},
				{ID: "http://10.0.0.1", Weight: 1, Inflight: 0, Available: false},
			},
			Health: []health.Status{
				{Target: "http://10.0.0.1", Available: false, ConsecutiveFailures: 4, LastError: "dial refused", LastChange: changed},
				{Target: "http://10.0.0.2", Available: true, ConsecutiveFailures: 0, LastChange: changed},
			},
			Breaker: &breaker.Snapshot{
				State:               breaker.Open,
				OpenedAt:            changed,
				ConsecutiveFailures: 5,
				WindowSuccesses:     1,
				WindowFailures:      3,
				WindowRequests:      4,
				FailureRatio:        0.75,
			},
		}},
	}
}

func newTestHandler(t *testing.T, src Source) (*Handler, *clock.Fake) {
	t.Helper()
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	h, err := New(Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     c,
		Source:    src,
		Token:     testToken,
		Version:   "v9.9.9",
		StartedAt: time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, c
}

func do(t *testing.T, h http.Handler, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("json: %v (%s)", err, rec.Body.String())
	}
}

func errorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeJSON(t, rec, &body)
	return body.Error.Code, body.Error.Message
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{Source: &fakeSource{}}); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := New(Deps{Token: "   ", Source: &fakeSource{}}); err == nil {
		t.Fatal("blank token accepted")
	}
	if _, err := New(Deps{Token: testToken}); err == nil {
		t.Fatal("nil source accepted")
	}
	h, err := New(Deps{Token: testToken, Source: &fakeSource{}, Version: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h.Version() != "dev" {
		t.Fatalf("version = %q", h.Version())
	}
}

func TestProbesAreUnauthenticated(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ready: true}
	h, _ := newTestHandler(t, src)

	rec := do(t, h, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	var healthBody map[string]string
	decodeJSON(t, rec, &healthBody)
	if healthBody["status"] != "ok" {
		t.Fatalf("health body = %v", healthBody)
	}

	rec = do(t, h, http.MethodGet, "/readyz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d", rec.Code)
	}
	var readyBody map[string]any
	decodeJSON(t, rec, &readyBody)
	if readyBody["status"] != "ready" || readyBody["ready"] != true {
		t.Fatalf("ready body = %v", readyBody)
	}

	src.ready = false
	src.reason = "redis unavailable"
	rec = do(t, h, http.MethodGet, "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", rec.Code)
	}
	readyBody = nil
	decodeJSON(t, rec, &readyBody)
	if readyBody["status"] != "not_ready" || readyBody["reason"] != "redis unavailable" {
		t.Fatalf("not-ready body = %v", readyBody)
	}

	rec = do(t, h, http.MethodPost, "/healthz", "", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("health method = %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, &fakeSource{ready: true})
	cases := []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"wrong token", "Bearer nope"},
		{"wrong scheme", "Basic " + testToken},
		{"empty bearer", "Bearer "},
		{"prefix only", "Bearer"},
		{"token prefix", "Bearer s3cret-admin"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate")
			}
			code, _ := errorEnvelope(t, rec)
			if code != httpx.CodeUnauthorized {
				t.Fatalf("code = %q", code)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("Authorization", "bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lowercase scheme status = %d", rec.Code)
	}
}

func TestOverview(t *testing.T) {
	t.Parallel()
	src := &fakeSource{snapshot: snapshotFixture(), ready: true}
	h, c := newTestHandler(t, src)
	c.Advance(95 * time.Second)

	rec := do(t, h, http.MethodGet, "/admin/api/overview", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %v", rec.Header())
	}
	var got overview
	decodeJSON(t, rec, &got)

	if got.Version != "v9.9.9" || got.UptimeSeconds != 95 || !got.Ready || got.RateLimiter != "redis" {
		t.Fatalf("overview header = %+v", got)
	}
	if got.Reloads != 2 || got.StartedAt == "" {
		t.Fatalf("reload metadata = %+v", got)
	}
	if len(got.Routes) != 1 {
		t.Fatalf("routes = %+v", got.Routes)
	}
	route := got.Routes[0]
	if route.Name != "api" || route.Host != "api.example" || route.PathPrefix != "/v1" ||
		route.Upstream != "api" || !route.AuthRequired || route.Timeout != "5s" ||
		route.MaxBodyBytes != 4096 || route.RateLimit == nil {
		t.Fatalf("route = %+v", route)
	}
	if route.RateLimit.Capacity != 40 || route.RateLimit.RefillPerSec != 20 ||
		route.RateLimit.Key != "ip" || !route.RateLimit.FailOpen {
		t.Fatalf("rate limit = %+v", route.RateLimit)
	}
	if len(got.Upstreams) != 1 {
		t.Fatalf("upstreams = %+v", got.Upstreams)
	}
	up := got.Upstreams[0]
	if up.Name != "api" || up.Balancer != "least_conn" || len(up.Targets) != 2 {
		t.Fatalf("upstream = %+v", up)
	}
	if up.Targets[0].URL != "http://10.0.0.1" || up.Targets[0].Available ||
		up.Targets[0].ConsecutiveFailures != 4 || up.Targets[0].LastError != "dial refused" {
		t.Fatalf("sorted target = %+v", up.Targets[0])
	}
	if up.Targets[1].URL != "http://10.0.0.2" || !up.Targets[1].Available || up.Targets[1].Inflight != 3 {
		t.Fatalf("target = %+v", up.Targets[1])
	}
	if up.Breaker == nil || up.Breaker.State != "open" || up.Breaker.FailureRatio != 0.75 ||
		up.Breaker.WindowFailures != 3 || up.Breaker.WindowRequests != 4 ||
		up.Breaker.ConsecutiveFailures != 5 || up.Breaker.OpenedAt == "" {
		t.Fatalf("breaker = %+v", up.Breaker)
	}

	rec = do(t, h, http.MethodGet, "/admin/api/upstreams", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("upstreams status = %d", rec.Code)
	}
	var upBody struct {
		Count     int            `json:"count"`
		Upstreams []upstreamView `json:"upstreams"`
	}
	decodeJSON(t, rec, &upBody)
	if upBody.Count != 1 || len(upBody.Upstreams) != 1 {
		t.Fatalf("upstreams body = %+v", upBody)
	}
}

func TestOverviewWithoutConfigAndNotReady(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ready: false, reason: "starting"}
	h, _ := newTestHandler(t, src)
	rec := do(t, h, http.MethodGet, "/admin/api/overview", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got overview
	decodeJSON(t, rec, &got)
	if got.Ready || got.ReadinessReason != "starting" {
		t.Fatalf("readiness = %+v", got)
	}
	if got.Routes == nil || len(got.Routes) != 0 || got.Upstreams == nil || len(got.Upstreams) != 0 {
		t.Fatalf("empty slices = %+v", got)
	}

	rec = do(t, h, http.MethodGet, "/admin/api/config", testToken, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("config status = %d", rec.Code)
	}
}

func TestConfigEndpointRedactsAPIKeys(t *testing.T) {
	t.Parallel()
	snap := snapshotFixture()
	hash := strings.Repeat("ab", 32)
	snap.Config.Auth.APIKeys = []config.APIKey{{ID: "k1", Hash: hash, Scopes: []string{"read"}}}
	src := &fakeSource{snapshot: snap, ready: true}
	h, _ := newTestHandler(t, src)

	rec := do(t, h, http.MethodGet, "/admin/api/config", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/yaml") {
		t.Fatalf("content type = %q", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, hash) {
		t.Fatalf("API key hash leaked:\n%s", body)
	}
	if !strings.Contains(body, "[REDACTED]") || !strings.Contains(body, "api_keys") {
		t.Fatalf("redacted config missing markers:\n%s", body)
	}
	if src.snapshot.Config.Auth.APIKeys[0].Hash != hash {
		t.Fatal("config endpoint mutated the live snapshot")
	}
}

func TestReload(t *testing.T) {
	t.Parallel()
	src := &fakeSource{snapshot: snapshotFixture(), ready: true}
	h, _ := newTestHandler(t, src)

	rec := do(t, h, http.MethodPost, "/admin/reload", testToken, bytes.NewReader([]byte("ignored")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status   string `json:"status"`
		Reloads  uint64 `json:"reloads"`
		Last     string `json:"last_reload"`
		ErrorMsg string `json:"error"`
	}
	decodeJSON(t, rec, &body)
	if body.Status != "reloaded" || body.Reloads != 3 || body.Last == "" {
		t.Fatalf("body = %+v", body)
	}
	if src.reloadCall != 1 {
		t.Fatalf("reload calls = %d", src.reloadCall)
	}

	src.reloadErr = errors.New("invalid config: boom")
	rec = do(t, h, http.MethodPost, "/admin/reload", testToken, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failure status = %d", rec.Code)
	}
	code, message := errorEnvelope(t, rec)
	if code != httpx.CodeInternalError || !strings.Contains(message, "previous configuration") {
		t.Fatalf("failure envelope = %q %q", code, message)
	}
	if strings.Contains(message, "boom") {
		t.Fatal("internal reload error leaked to the response")
	}

	rec = do(t, h, http.MethodGet, "/admin/reload", testToken, nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET reload = %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestDashboard(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, &fakeSource{ready: true})

	rec := do(t, h, http.MethodGet, "/dashboard", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{
		"<!doctype html>",
		"Gateway operations",
		"/admin/api/overview",
		"/admin/reload",
		"sessionStorage",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("dashboard missing %q", marker)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}

	rec = do(t, h, http.MethodHead, "/dashboard/", "", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %d bytes", rec.Code, rec.Body.Len())
	}

	rec = do(t, h, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("root = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = do(t, h, http.MethodPost, "/dashboard", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST dashboard = %d", rec.Code)
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, &fakeSource{ready: true})

	rec := do(t, h, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	code, _ := errorEnvelope(t, rec)
	if code != httpx.CodeRouteNotFound {
		t.Fatalf("code = %q", code)
	}

	rec = do(t, h, http.MethodPost, "/admin/api/overview", testToken, nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("overview method = %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	code, _ = errorEnvelope(t, rec)
	if code != httpx.CodeMethodNotAllowed {
		t.Fatalf("code = %q", code)
	}

	rec = do(t, h, http.MethodGet, "/admin/api/upstreams", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("upstreams without auth = %d", rec.Code)
	}
}

func TestPprofRequiresToken(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, &fakeSource{ready: true})

	rec := do(t, h, http.MethodGet, "/debug/pprof/", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized pprof status = %d", rec.Code)
	}

	rec = do(t, h, http.MethodGet, "/debug/pprof/", testToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pprof status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "profile") {
		t.Fatalf("pprof index body = %s", rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/debug/pprof/cmdline", testToken, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("cmdline = %d %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestUptimeClampedAndRound(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ready: true}
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	h, err := New(Deps{
		Clock:     c,
		Source:    src,
		Token:     testToken,
		StartedAt: time.Unix(1_700_000_100, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, http.MethodGet, "/admin/api/overview", testToken, nil)
	var got overview
	decodeJSON(t, rec, &got)
	if got.UptimeSeconds != 0 {
		t.Fatalf("uptime = %v", got.UptimeSeconds)
	}
	if round(0.12345) != 0.123 || round(0.12365) != 0.124 {
		t.Fatalf("round = %v %v", round(0.12345), round(0.12365))
	}
}
