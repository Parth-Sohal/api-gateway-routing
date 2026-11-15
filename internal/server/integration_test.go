package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

func TestIntegrationFullStack(t *testing.T) {
	const (
		adminEnv = "GATEWAY_IT_ADMIN_TOKEN"
		jwtEnv   = "GATEWAY_IT_JWT_SECRET"
		jwtKey   = "integration-jwt-secret"
	)
	t.Setenv(adminEnv, "integration-admin-token")
	t.Setenv(jwtEnv, jwtKey)

	upstream, echo := newEchoUpstream(t, "users-a")
	cfg := mustParse(t, fullConfig(adminEnv, jwtEnv, fmt.Sprintf(`
  - name: api
    targets:
      - url: %s
`, upstream.URL), `
  - name: api
    match:
      path_prefix: /api
    strip_prefix: /api
    upstream: api
    timeout: 5s
    auth:
      required: true
      methods: [jwt, api_key]
      match: any
      scopes: [users:read]
`))
	s := startE2EServer(t, cfg, "")
	client := newHTTPClient(t)

	// Unauthenticated requests are rejected without reaching the upstream.
	resp, raw := doRequest(t, client, http.MethodGet, s.Public+"/api/hello", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body = %s", resp.StatusCode, raw)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("missing WWW-Authenticate: %v", resp.Header)
	}
	if code := errorCode(t, raw); code != httpx.CodeUnauthorized {
		t.Fatalf("error code = %q", code)
	}
	if resp.Header.Get(httpx.HeaderRequestID) == "" {
		t.Fatal("response has no request ID")
	}

	// A valid JWT is authorized, stripped path and identity reach the upstream,
	// and the correlation ID survives the whole chain.
	jwtToken := mintJWT(t, jwtKey, "jwt-user", "users:read", "users:write")
	resp, raw = doRequest(t, client, http.MethodGet, s.Public+"/api/hello?x=1", "", map[string]string{
		"Authorization":       "Bearer " + jwtToken,
		httpx.HeaderRequestID: "e2e-correlation-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("JWT status = %d, body = %s", resp.StatusCode, raw)
	}
	body := decodeMap(t, raw)
	if body["upstream"] != "users-a" || body["path"] != "/hello" {
		t.Fatalf("upstream payload = %v", body)
	}
	if body["request_id"] != "e2e-correlation-1" || resp.Header.Get(httpx.HeaderRequestID) != "e2e-correlation-1" {
		t.Fatalf("request ID = body %v header %q", body["request_id"], resp.Header.Get(httpx.HeaderRequestID))
	}
	if body["user_id"] != "jwt-user" || !strings.Contains(body["scopes"].(string), "users:read") {
		t.Fatalf("identity = %v", body)
	}

	// API-key authentication uses the same route policy.
	resp, raw = doRequest(t, client, http.MethodGet, s.Public+"/api/hello", "", map[string]string{
		"Authorization": "Bearer gw_demo_demo-secret-123",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("API key status = %d, body = %s", resp.StatusCode, raw)
	}
	if got := decodeMap(t, raw)["user_id"]; got != "demo" {
		t.Fatalf("API-key user = %v", got)
	}

	// Authenticated but missing the required scope.
	noScope := mintJWT(t, jwtKey, "jwt-user", "orders:read")
	resp, raw = doRequest(t, client, http.MethodGet, s.Public+"/api/hello", "", map[string]string{
		"Authorization": "Bearer " + noScope,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scope failure status = %d, body = %s", resp.StatusCode, raw)
	}
	if code := errorCode(t, raw); code != httpx.CodeForbidden {
		t.Fatalf("scope error code = %q", code)
	}

	if echo.requests.Load() != 2 {
		t.Fatalf("upstream saw %d requests, want 2", echo.requests.Load())
	}

	// Admin probes are unauthenticated; the API requires the bearer token.
	resp, raw = doRequest(t, client, http.MethodGet, s.Admin+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, body = %s", resp.StatusCode, raw)
	}
	resp, raw = doRequest(t, client, http.MethodGet, s.Admin+"/readyz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d, body = %s", resp.StatusCode, raw)
	}
	resp, _ = doRequest(t, client, http.MethodGet, s.Admin+"/admin/api/overview", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("overview without token = %d", resp.StatusCode)
	}
	resp, raw = doRequest(t, client, http.MethodGet, s.Admin+"/admin/api/overview", "", map[string]string{
		"Authorization": "Bearer integration-admin-token",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview = %d, body = %s", resp.StatusCode, raw)
	}
	overview := decodeMap(t, raw)
	if overview["ready"] != true {
		t.Fatalf("overview not ready: %v", overview)
	}
	if routes, _ := overview["routes"].([]any); len(routes) != 1 {
		t.Fatalf("overview routes = %v", overview["routes"])
	}
	if upstreams, _ := overview["upstreams"].([]any); len(upstreams) != 1 {
		t.Fatalf("overview upstreams = %v", overview["upstreams"])
	}

	// The config endpoint redacts credential material.
	resp, raw = doRequest(t, client, http.MethodGet, s.Admin+"/admin/api/config", "", map[string]string{
		"Authorization": "Bearer integration-admin-token",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config = %d, body = %s", resp.StatusCode, raw)
	}
	rendered := string(raw)
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("config hash was not redacted:\n%s", rendered)
	}
	if strings.Contains(rendered, "264c5c83f07d30c7ccc1083d7e416705272eafa1fc15167e09bed57b7697faed") {
		t.Fatalf("config leaked the API-key hash:\n%s", rendered)
	}
	if strings.Contains(rendered, "demo-secret-123") {
		t.Fatalf("config leaked the API-key secret:\n%s", rendered)
	}

	// Metrics include HTTP, auth-failure and build series.
	resp, raw = doRequest(t, client, http.MethodGet, s.Metrics+"/metrics", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d", resp.StatusCode)
	}
	scrape := string(raw)
	for _, want := range []string{
		"gateway_http_requests_total",
		"gateway_auth_failures_total",
		`gateway_build_info{version="e2e"}`,
		"gateway_target_healthy",
	} {
		if !strings.Contains(scrape, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

func TestIntegrationFailoverAcrossTargets(t *testing.T) {
	// A target that always returns 503.
	var badHits atomic.Int64
	badServer := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		http.Error(w, "injected failure", http.StatusServiceUnavailable)
	}))

	// A target that accepts and immediately resets the connection.
	deadListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var deadAccepts atomic.Int64
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := deadListener.Accept()
			if err != nil {
				return
			}
			deadAccepts.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = deadListener.Close()
		<-acceptDone
	})
	deadURL := "http://" + deadListener.Addr().String()

	// Two scenarios instead of one three-target pool: the retry budget
	// deposits one token per request, so each request may use at most one
	// retry without starving later requests. Each scenario therefore has one
	// failing target followed by a healthy one.
	scenarios := []struct {
		name    string
		failing string
		hits    *atomic.Int64
	}{
		{"503", badServer.URL, &badHits},
		{"connection error", deadURL, &deadAccepts},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			good, echo := newEchoUpstream(t, "good")
			cfg := mustParse(t, minimalConfig(fmt.Sprintf(`
  - name: failover
    balancer: round_robin
    targets:
      - url: %s
      - url: %s
    transport:
      dial_timeout: 200ms
    health_check:
      enabled: false
    breaker:
      enabled: false
    retry:
      enabled: true
      max_attempts: 3
      on: [connection_error, 503]
      base_backoff: 2ms
      max_backoff: 10ms
      budget_percent: 100
`, tc.failing, good.URL), `
  - name: failover
    match:
      path_prefix: /
    upstream: failover
    timeout: 5s
`))
			s := startE2EServer(t, cfg, "")
			client := newHTTPClient(t)

			before := tc.hits.Load()
			for i := 0; i < 40; i++ {
				resp, raw := doRequest(t, client, http.MethodGet, s.Public+"/load", "", nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("request %d status = %d, body = %s", i, resp.StatusCode, raw)
				}
				if got := decodeMap(t, raw)["upstream"]; got != "good" {
					t.Fatalf("request %d reached %v", i, got)
				}
			}
			if tc.hits.Load() == before {
				t.Fatalf("the %s target was never attempted; failover was not exercised", tc.name)
			}
			if echo.requests.Load() == 0 {
				t.Fatal("the healthy target never served a request")
			}
		})
	}
}

func TestIntegrationReloadSwapsAndKeepsOldOnFailure(t *testing.T) {
	upstreamA, _ := newEchoUpstream(t, "a")
	upstreamB, _ := newEchoUpstream(t, "b")

	configFor := func(target string) string {
		return minimalConfig(fmt.Sprintf(`
  - name: version
    targets:
      - url: %s
`, target), `
  - name: version
    match:
      path_prefix: /version
    upstream: version
    timeout: 3s
`)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(configFor(upstreamA.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustParse(t, configFor(upstreamA.URL))
	s := startE2EServer(t, cfg, path)
	client := newHTTPClient(t)

	current := func() string {
		resp, raw := doRequest(t, client, http.MethodGet, s.Public+"/version", "", nil)
		if resp.StatusCode != http.StatusOK {
			return ""
		}
		value, _ := decodeMap(t, raw)["upstream"].(string)
		return value
	}
	if got := current(); got != "a" {
		t.Fatalf("initial upstream = %q", got)
	}

	// The watcher debounces file changes and swaps a new immutable snapshot.
	if err := os.WriteFile(path, []byte(configFor(upstreamB.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "reload to upstream b", func() bool { return current() == "b" })

	// A broken file must leave the previous snapshot serving.
	if err := os.WriteFile(path, []byte("routes: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(800 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if got := current(); got != "b" {
			t.Fatalf("after failed reload upstream = %q, want b", got)
		}
	}

	// Restoring the file and reloading explicitly recovers.
	if err := os.WriteFile(path, []byte(configFor(upstreamA.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("explicit reload: %v", err)
	}
	waitFor(t, 3*time.Second, "recovery to upstream a", func() bool { return current() == "a" })
}
