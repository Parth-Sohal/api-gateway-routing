package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func writeConfigFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func gatewayConfig(targetURL, routeBlock string) string {
	return fmt.Sprintf(`
server:
  addr: 127.0.0.1:0
  shutdown_grace: 5s
admin:
  enabled: true
  addr: 127.0.0.1:0
  token_env: GATEWAY_TEST_ADMIN_TOKEN
  shutdown_grace: 2s
metrics:
  enabled: true
  addr: 127.0.0.1:0
  path: /metrics
log:
  level: error
  access_log: false
upstreams:
  - name: api
    targets:
      - url: %q
routes:
%s
`, targetURL, routeBlock)
}

func startTestServer(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
}

func get(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func adminURL(s *Server, path string) string { return "http://" + s.Addrs().Admin + path }

func bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

func TestServerStartServeReloadAndShutdown(t *testing.T) {
	t.Setenv("GATEWAY_TEST_ADMIN_TOKEN", "admin-secret")
	dir := t.TempDir()

	var hits atomic.Int64
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-Upstream", "api")
		_, _ = io.WriteString(w, "hello from upstream")
	})
	path := writeConfigFile(t, dir, "gateway.yaml", gatewayConfig(up.URL, `
  - name: api
    match:
      path_prefix: /api
    upstream: api
    strip_prefix: /api
    timeout: 3s
`))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), Version: "vtest", DisableWatch: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ready, reason := s.Ready(); ready || reason != "starting" {
		t.Fatalf("readiness before Start = %v %q", ready, reason)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	addrs := s.Addrs()
	if addrs.Public == "" || addrs.Admin == "" || addrs.Metrics == "" {
		t.Fatalf("addrs = %+v", addrs)
	}
	if ready, reason := s.Ready(); !ready || reason != "" {
		t.Fatalf("readiness = %v %q", ready, reason)
	}

	resp := get(t, "http://"+addrs.Public+"/api/hello", nil)
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "hello from upstream" {
		t.Fatalf("public response = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "api" || resp.Header.Get(httpx.HeaderRequestID) == "" {
		t.Fatalf("public headers = %v", resp.Header)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}

	resp = get(t, "http://"+addrs.Public+"/missing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing route = %d", resp.StatusCode)
	}

	// Probes are unauthenticated, the API is not.
	resp = get(t, adminURL(s, "/healthz"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	resp = get(t, adminURL(s, "/readyz"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d", resp.StatusCode)
	}
	resp = get(t, adminURL(s, "/admin/api/overview"), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated overview = %d", resp.StatusCode)
	}
	resp = get(t, adminURL(s, "/admin/api/overview"), bearer("wrong"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token overview = %d", resp.StatusCode)
	}
	resp = get(t, adminURL(s, "/admin/api/overview"), bearer("admin-secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview = %d %s", resp.StatusCode, readBody(t, resp))
	}
	var overview struct {
		Version   string `json:"version"`
		Ready     bool   `json:"ready"`
		Reloads   uint64 `json:"reloads"`
		RateLimit string `json:"rate_limiter"`
		Routes    []struct {
			Name string `json:"name"`
		} `json:"routes"`
		Upstreams []struct {
			Name    string `json:"name"`
			Targets []struct {
				URL       string `json:"url"`
				Available bool   `json:"available"`
			} `json:"targets"`
		} `json:"upstreams"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &overview); err != nil {
		t.Fatalf("overview json: %v", err)
	}
	if overview.Version != "vtest" || !overview.Ready || overview.Reloads != 0 {
		t.Fatalf("overview = %+v", overview)
	}
	if len(overview.Routes) != 1 || overview.Routes[0].Name != "api" {
		t.Fatalf("routes = %+v", overview.Routes)
	}
	if len(overview.Upstreams) != 1 || len(overview.Upstreams[0].Targets) != 1 ||
		!overview.Upstreams[0].Targets[0].Available {
		t.Fatalf("upstreams = %+v", overview.Upstreams)
	}

	// Metrics listener serves the private registry and records the request.
	resp = get(t, "http://"+addrs.Metrics+"/metrics", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `gateway_http_requests_total{method="GET",route="api",status="200"} 1`) {
		t.Fatalf("metrics = %d\n%s", resp.StatusCode, firstLines(body, 20))
	}

	// Admin reload rebuilds the same config and increments the counter.
	req, err := http.NewRequest(http.MethodPost, adminURL(s, "/admin/reload"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = bearer("admin-secret")
	resp, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloadBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(reloadBody, `"status":"reloaded"`) {
		t.Fatalf("reload = %d %s", resp.StatusCode, reloadBody)
	}
	snap := s.AdminSnapshot()
	if snap.Reloads != 1 || snap.LastReload.IsZero() || snap.LastReloadError != "" {
		t.Fatalf("snapshot = %+v", snap)
	}

	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Start = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after Shutdown")
	}
	if ready, reason := s.Ready(); ready || reason != "shutting down" {
		t.Fatalf("readiness after Shutdown = %v %q", ready, reason)
	}
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("Reload succeeded after Shutdown")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded after Shutdown")
	}
}

func TestServerReloadSwapsStateAndKeepsOldOnFailure(t *testing.T) {
	t.Setenv("GATEWAY_TEST_ADMIN_TOKEN", "reload-secret")
	dir := t.TempDir()
	upA := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "A")
	})
	upB := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "B")
	})

	base := func(target, route string) string {
		return gatewayConfig(target, route)
	}
	path := writeConfigFile(t, dir, "gateway.yaml", base(upA.URL, `
  - name: api
    match:
      path_prefix: /v1
    upstream: api
    timeout: 3s
`))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startTestServer(t, s)

	resp := get(t, "http://"+s.Addrs().Public+"/v1/x", nil)
	if readBody(t, resp) != "A" {
		t.Fatalf("initial route body = %q", readBody(t, resp))
	}

	// A new file is picked up only on reload; the old route disappears.
	writeConfigFile(t, dir, "gateway.yaml", base(upB.URL, `
  - name: api
    match:
      path_prefix: /v2
    upstream: api
    timeout: 3s
`))
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	snap := s.AdminSnapshot()
	if snap.Reloads != 1 || snap.LastReloadError != "" || snap.Config == nil {
		t.Fatalf("snapshot after reload = %+v", snap)
	}
	resp = get(t, "http://"+s.Addrs().Public+"/v2/x", nil)
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "B" {
		t.Fatalf("new route = %d", resp.StatusCode)
	}
	resp = get(t, "http://"+s.Addrs().Public+"/v1/x", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old route status = %d", resp.StatusCode)
	}

	writeConfigFile(t, dir, "gateway.yaml", "routes: [this is not valid\n")
	err = s.Reload(context.Background())
	if err == nil {
		t.Fatal("invalid reload succeeded")
	}
	snap = s.AdminSnapshot()
	if snap.Reloads != 1 || snap.LastReloadError == "" {
		t.Fatalf("snapshot after failed reload = %+v", snap)
	}
	resp = get(t, "http://"+s.Addrs().Public+"/v2/x", nil)
	if resp.StatusCode != http.StatusOK || readBody(t, resp) != "B" {
		t.Fatal("failed reload replaced the serving state")
	}
	if ready, _ := s.Ready(); !ready {
		t.Fatal("gateway not ready after failed reload")
	}

	// Reload without a config path is rejected without disturbing traffic.
	noPath, err := New(Options{Config: snap.Config, Logger: testLogger(), Metrics: metrics.New("test")})
	if err != nil {
		t.Fatal(err)
	}
	startTestServer(t, noPath)
	if err := noPath.Reload(context.Background()); err == nil {
		t.Fatal("reload without a path succeeded")
	}
}

func TestServerGracefulDrainCompletesInflight(t *testing.T) {
	t.Setenv("GATEWAY_TEST_ADMIN_TOKEN", "drain-secret")
	dir := t.TempDir()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = io.WriteString(w, "drained")
	})
	path := writeConfigFile(t, dir, "gateway.yaml", gatewayConfig(up.URL, `
  - name: api
    match:
      path_prefix: /slow
    upstream: api
    timeout: 5s
`))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	startTestServer(t, s)

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + s.Addrs().Public + "/slow")
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		ready, reason := s.Ready()
		if !ready && reason == "shutting down" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness did not drop: %v %q", ready, reason)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// New connections are refused while the in-flight request keeps its
	// connection and completes normally.
	if _, err := (&http.Client{Timeout: 200 * time.Millisecond}).Get("http://" + s.Addrs().Public + "/slow"); err == nil {
		t.Fatal("new request succeeded during drain")
	}
	close(release)

	select {
	case err := <-errCh:
		t.Fatalf("in-flight request: %v", err)
	case resp := <-respCh:
		if resp.StatusCode != http.StatusOK || readBody(t, resp) != "drained" {
			t.Fatalf("in-flight response = %d", resp.StatusCode)
		}
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the in-flight request completed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServerDisabledListenersAndStartErrors(t *testing.T) {
	dir := t.TempDir()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// Admin and metrics can be disabled; no admin token is needed.
	path := writeConfigFile(t, dir, "minimal.yaml", fmt.Sprintf(`
server:
  addr: 127.0.0.1:0
admin:
  enabled: false
metrics:
  enabled: false
log:
  access_log: false
upstreams:
  - name: api
    targets:
      - url: %q
routes:
  - name: api
    match:
      path_prefix: /
    upstream: api
`, up.URL))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	startTestServer(t, s)
	addrs := s.Addrs()
	if addrs.Public == "" || addrs.Admin != "" || addrs.Metrics != "" {
		t.Fatalf("addrs = %+v", addrs)
	}
	resp := get(t, "http://"+addrs.Public+"/x", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// An enabled admin listener without its token fails closed.
	t.Setenv("GATEWAY_TEST_MISSING_TOKEN", "")
	path = writeConfigFile(t, dir, "admin.yaml", fmt.Sprintf(`
server:
  addr: 127.0.0.1:0
admin:
  enabled: true
  addr: 127.0.0.1:0
  token_env: GATEWAY_TEST_MISSING_TOKEN
metrics:
  enabled: false
upstreams:
  - name: api
    targets:
      - url: %q
routes:
  - name: api
    match:
      path_prefix: /
    upstream: api
`, up.URL))
	failed, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "GATEWAY_TEST_MISSING_TOKEN") {
		t.Fatalf("Start without admin token = %v", err)
	}
	if err := failed.Start(context.Background()); err == nil {
		t.Fatal("Start retried after a failed Start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := failed.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after failed Start: %v", err)
	}
	select {
	case <-failed.Done():
	default:
		t.Fatal("Done not closed after failed Start shutdown")
	}

	// Invalid listen addresses fail Start.
	path = writeConfigFile(t, dir, "badaddr.yaml", fmt.Sprintf(`
server:
  addr: 127.0.0.1:notaport
admin:
  enabled: false
metrics:
  enabled: false
upstreams:
  - name: api
    targets:
      - url: %q
routes:
  - name: api
    match:
      path_prefix: /
    upstream: api
`, up.URL))
	bad, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen public") {
		t.Fatalf("Start with bad addr = %v", err)
	}
	if err := bad.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after bad listen: %v", err)
	}

	// Constructing without config, and starting with a cancelled context.
	if _, err := New(Options{Logger: testLogger()}); err == nil {
		t.Fatal("New without config succeeded")
	}
	if _, err := New(Options{ConfigPath: filepath.Join(dir, "absent.yaml")}); err == nil {
		t.Fatal("New with missing config succeeded")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	cancelled, err := New(Options{ConfigPath: filepath.Join(dir, "minimal.yaml"), Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Start(ctx2); err == nil {
		t.Fatal("Start with cancelled context succeeded")
	}
	if err := cancelled.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after cancelled Start: %v", err)
	}
}

func TestServerRunExitsOnContext(t *testing.T) {
	dir := t.TempDir()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	path := writeConfigFile(t, dir, "gateway.yaml", fmt.Sprintf(`
server:
  addr: 127.0.0.1:0
  shutdown_grace: 2s
admin:
  enabled: false
metrics:
  enabled: false
log:
  access_log: false
upstreams:
  - name: api
    targets:
      - url: %q
routes:
  - name: api
    match:
      path_prefix: /
    upstream: api
`, up.URL))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if ready, _ := s.Ready(); ready {
		t.Fatal("ready before Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready, _ := s.Ready(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not exit")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed")
	}
}

func TestServerReloadConcurrentWithTraffic(t *testing.T) {
	t.Setenv("GATEWAY_TEST_ADMIN_TOKEN", "load-secret")
	dir := t.TempDir()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	path := writeConfigFile(t, dir, "gateway.yaml", gatewayConfig(up.URL, `
  - name: api
    match:
      path_prefix: /api
    upstream: api
    timeout: 3s
`))
	s, err := New(Options{ConfigPath: path, Logger: testLogger(), Metrics: metrics.New("test"), Version: "load", DisableWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	startTestServer(t, s)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := client.Get("http://" + s.Addrs().Public + "/api/x")
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if err := s.Reload(context.Background()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	client.CloseIdleConnections()
	if snap := s.AdminSnapshot(); snap.Reloads != 5 {
		t.Fatalf("reloads = %d", snap.Reloads)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
