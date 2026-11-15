package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/ratelimit"
)

func parseConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func newStateForTest(t *testing.T, cfg *config.Config, limiter ratelimit.Limiter, mode string) *state {
	t.Helper()
	st, err := newState(cfg, stateDeps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:     metrics.New("test"),
		Limiter:     limiter,
		LimiterMode: mode,
	})
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

type fakeLimiter struct {
	mu       sync.Mutex
	decision ratelimit.Decision
	err      error
	calls    int
	key      string
	rule     ratelimit.Rule
}

func (f *fakeLimiter) Allow(_ context.Context, key string, rule ratelimit.Rule) (ratelimit.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.key = key
	f.rule = rule
	return f.decision, f.err
}

func (f *fakeLimiter) last() (int, string, ratelimit.Rule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.key, f.rule
}

func renderMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func upstreamConfig(t *testing.T, url string, routeBlock string) *config.Config {
	t.Helper()
	return parseConfig(t, fmt.Sprintf(`
server:
  trusted_proxies: ["10.0.0.0/8"]
upstreams:
  - name: api
    targets:
      - url: %q
routes:
%s
`, url, routeBlock))
}

func upstreamConfigWithKeys(t *testing.T, url string, keys []config.APIKey, routeBlock string) *config.Config {
	t.Helper()
	var block strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&block, "    - id: %q\n      hash: %q\n", k.ID, k.Hash)
		if len(k.Scopes) > 0 {
			block.WriteString("      scopes: [" + strings.Join(k.Scopes, ", ") + "]\n")
		}
		if len(k.Roles) > 0 {
			block.WriteString("      roles: [" + strings.Join(k.Roles, ", ") + "]\n")
		}
	}
	return parseConfig(t, fmt.Sprintf(`
server:
  trusted_proxies: ["10.0.0.0/8"]
auth:
  api_keys:
%supstreams:
  - name: api
    targets:
      - url: %q
routes:
%s
`, block.String(), url, routeBlock))
}

func TestStateForwardsAndSetsIdentityHeaders(t *testing.T) {
	t.Parallel()
	var seen http.Header
	var path, body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen = r.Header.Clone()
		path = r.URL.Path
		body = string(data)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)

	cfg := upstreamConfigWithKeys(t, up.URL, []config.APIKey{
		{ID: "demo", Hash: hashOf("secret"), Scopes: []string{"read"}},
	}, `
  - name: api
    match:
      path_prefix: /api
    upstream: api
    strip_prefix: /api
    auth:
      required: true
`)
	st := newStateForTest(t, cfg, nil, "disabled")

	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/api/items?q=1", strings.NewReader("payload"))
	req.RemoteAddr = "192.0.2.7:1234"
	req.Header.Set("Authorization", "Bearer gw_demo_secret")
	req.Header.Set("X-User-Id", "spoofed")
	req.Header.Set("X-Forwarded-User", "spoofed")
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != "ok" || rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("response = %d %q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}
	if path != "/items" || body != "payload" {
		t.Fatalf("upstream saw path=%q body=%q", path, body)
	}
	if seen.Get("X-User-Id") != "demo" || seen.Get("X-User-Kind") != "api_key" ||
		seen.Get("X-User-Scopes") != "read" || seen.Get("X-Forwarded-User") != "" {
		t.Fatalf("identity headers = %v", seen)
	}
	if seen.Get("X-Forwarded-For") != "192.0.2.7" {
		t.Fatalf("xff = %q", seen.Get("X-Forwarded-For"))
	}
	// Untrusted proxies are ignored; the socket peer is the client.
	if rec.Header().Get(httpx.HeaderRequestID) == "" {
		t.Fatal("missing response request ID")
	}
}

func TestStateStripsSpoofedIdentityWithoutAuth(t *testing.T) {
	t.Parallel()
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(up.Close)

	cfg := upstreamConfig(t, up.URL, `
  - name: api
    match:
      path_prefix: /
    upstream: api
`)
	st := newStateForTest(t, cfg, nil, "disabled")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Principal", "admin")
	req.Header.Set("X-User-Roles", "root")
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen.Get("X-Principal") != "" || seen.Get("X-User-Roles") != "" || seen.Get("X-User-Id") != "" {
		t.Fatalf("identity headers leaked: %v", seen)
	}
}

func TestStateAuthFailures(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := upstreamConfigWithKeys(t, up.URL, []config.APIKey{
		{ID: "reader", Hash: hashOf("secret"), Scopes: []string{"read"}},
		{ID: "writer", Hash: hashOf("secret"), Scopes: []string{"write"}, Roles: []string{"admin"}},
	}, `
  - name: api
    match:
      path_prefix: /api
    upstream: api
    auth:
      required: true
      scopes: ["write"]
      roles: ["admin"]
`)
	st := newStateForTest(t, cfg, nil, "disabled")

	cases := []struct {
		name   string
		header string
		status int
		code   string
	}{
		{"missing", "", http.StatusUnauthorized, httpx.CodeUnauthorized},
		{"invalid", "Bearer gw_writer_wrong", http.StatusUnauthorized, httpx.CodeUnauthorized},
		{"insufficient scope", "Bearer gw_reader_secret", http.StatusForbidden, httpx.CodeForbidden},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			st.ServeHTTP(rec, req)
			if rec.Code != c.status || errCode(t, rec) != c.code {
				t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
			}
			if c.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing WWW-Authenticate")
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer gw_writer_secret")
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized response = %d %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}

	body := renderMetrics(t, st.metrics)
	for _, want := range []string{
		`gateway_auth_failures_total{reason="missing",route="api"} 1`,
		`gateway_auth_failures_total{reason="invalid",route="api"} 1`,
		`gateway_auth_failures_total{reason="forbidden",route="api"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}
}

func TestStateRoutingErrorsAndPathCleaning(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	cfg := upstreamConfig(t, up.URL, `
  - name: api
    match:
      path_prefix: /api
      methods: ["GET"]
    upstream: api
`)
	st := newStateForTest(t, cfg, nil, "disabled")

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || errCode(t, rec) != httpx.CodeRouteNotFound {
		t.Fatalf("404 = %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/x", nil)
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || errCode(t, rec) != httpx.CodeMethodNotAllowed {
		t.Fatalf("405 = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}

	req = httptest.NewRequest(http.MethodGet, "/api/../admin", nil)
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dot-segment path = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.URL.Path = "/api/\x00x"
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != httpx.CodeInvalidRequest {
		t.Fatalf("control-char path = %d %s", rec.Code, rec.Body.String())
	}

	if hits.Load() != 0 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}
}

func TestStateRateLimitDecisions(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(up.Close)
	cfg := upstreamConfig(t, up.URL, `
  - name: api
    match:
      path_prefix: /api
    upstream: api
    rate_limit:
      capacity: 5
      refill_per_sec: 2
      key: ip
`)
	limiter := &fakeLimiter{decision: ratelimit.Decision{
		Allowed: true, Limit: 5, Remaining: 4, ResetAfter: 1500 * time.Millisecond,
	}}
	st := newStateForTest(t, cfg, limiter, "test")

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.RemoteAddr = "192.0.2.9:5555"
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed response = %d %s", rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{
		"X-RateLimit-Limit":     "5",
		"X-RateLimit-Remaining": "4",
		"X-RateLimit-Reset":     "2",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	calls, key, rule := limiter.last()
	if calls != 1 || key != "rl:api:ip:192.0.2.9" {
		t.Fatalf("limiter calls=%d key=%q", calls, key)
	}
	if rule.Capacity != 5 || rule.RefillPerSec != 2 || !rule.FailOpen {
		t.Fatalf("rule = %+v", rule)
	}

	limiter.decision = ratelimit.Decision{
		Allowed: false, Limit: 5, Remaining: 0, RetryAfter: 1200 * time.Millisecond,
	}
	req = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.RemoteAddr = "192.0.2.9:5555"
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != httpx.CodeRateLimited {
		t.Fatalf("limited response = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "2" || rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("limited headers = %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "Retry after 2 seconds") {
		t.Fatalf("limited body = %s", rec.Body.String())
	}

	limiter.decision = ratelimit.Decision{Allowed: true, Degraded: true, Limit: 5, Remaining: 1}
	req = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("degraded response = %d", rec.Code)
	}

	limiter.err = errors.New("redis down")
	req = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != httpx.CodeLimiterUnavailable {
		t.Fatalf("limiter error response = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}

	body := renderMetrics(t, st.metrics)
	for _, want := range []string{
		`gateway_ratelimit_decisions_total{decision="allowed",route="api"} 1`,
		`gateway_ratelimit_decisions_total{decision="limited",route="api"} 1`,
		`gateway_ratelimit_decisions_total{decision="failopen",route="api"} 1`,
		`gateway_ratelimit_decisions_total{decision="error",route="api"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}
}

func TestStateRateLimitKeyTypes(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(up.Close)

	cases := []struct {
		name   string
		key    string
		header string
		req    func(*http.Request)
		want   string
	}{
		{"ip", "ip", "", func(*http.Request) {}, "rl:api:ip:192.0.2.9"},
		{"header", "header", "X-Tenant", func(r *http.Request) { r.Header.Set("X-Tenant", "acme inc") }, "rl:api:header:x-tenant:h:"},
		{"missing header falls back to ip", "header", "X-Tenant", func(*http.Request) {}, "rl:api:header:x-tenant:192.0.2.9"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := upstreamConfig(t, up.URL, fmt.Sprintf(`
  - name: api
    match:
      path_prefix: /api
    upstream: api
    rate_limit:
      capacity: 5
      refill_per_sec: 2
      key: %s
      header: %q
`, c.key, c.header))
			limiter := &fakeLimiter{decision: ratelimit.Decision{Allowed: true, Limit: 5, Remaining: 4}}
			st := newStateForTest(t, cfg, limiter, "test")
			req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			req.RemoteAddr = "192.0.2.9:5555"
			c.req(req)
			rec := httptest.NewRecorder()
			st.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rec.Code)
			}
			_, key, _ := limiter.last()
			if strings.HasPrefix(c.want, "rl:api:header:x-tenant:h:") {
				if !strings.HasPrefix(key, c.want) {
					t.Fatalf("key = %q, want prefix %q", key, c.want)
				}
			} else if key != c.want {
				t.Fatalf("key = %q, want %q", key, c.want)
			}
		})
	}
}

func TestStateMaxConcurrentRequests(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(up.Close)
	cfg := upstreamConfig(t, up.URL, `
  - name: api
    match:
      path_prefix: /
    upstream: api
`)
	cfg.Server.MaxConcurrentRequests = 1
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st := newStateForTest(t, cfg, nil, "disabled")
	st.sem <- struct{}{}

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != httpx.CodeOverloaded {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	<-st.sem
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
