package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

// testLogger discards output so integration failures stay readable. Set
// GATEWAY_TEST_LOG=1 temporarily when debugging a failing test.
func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type runningServer struct {
	*Server
	Public  string
	Admin   string
	Metrics string
}

// startE2EServer starts a server with the supplied config. path is optional
// and only needed for watcher-driven reload tests.
func startE2EServer(t *testing.T, cfg *config.Config, path string) *runningServer {
	t.Helper()
	s, err := New(Options{
		ConfigPath: path,
		Config:     cfg,
		Logger:     quietTestLogger(),
		Metrics:    metrics.New("e2e"),
		Version:    "e2e",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		_ = s.Shutdown(context.Background())
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("server shutdown: %v", err)
		}
		select {
		case <-s.Done():
		case <-time.After(15 * time.Second):
			t.Errorf("server did not close Done after shutdown")
		}
	})
	addrs := s.Addrs()
	return &runningServer{
		Server:  s,
		Public:  "http://" + addrs.Public,
		Admin:   "http://" + addrs.Admin,
		Metrics: "http://" + addrs.Metrics,
	}
}

func mustParse(t *testing.T, document string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse config: %v\n%s", err, document)
	}
	return cfg
}

const minimalConfigTemplate = `
server:
  addr: "127.0.0.1:0"
  shutdown_grace: 5s
admin:
  enabled: false
metrics:
  enabled: false
log:
  level: error
  format: text
  access_log: false
upstreams:
%s
routes:
%s
`

func minimalConfig(upstreams, routes string) string {
	return fmt.Sprintf(minimalConfigTemplate, upstreams, routes)
}

const fullConfigTemplate = `
server:
  addr: "127.0.0.1:0"
  shutdown_grace: 5s
admin:
  enabled: true
  addr: "127.0.0.1:0"
  token_env: %s
metrics:
  enabled: true
  addr: "127.0.0.1:0"
log:
  level: error
  format: text
  access_log: false
auth:
  jwt:
    algorithms: [HS256]
    hs256_secret_env: %s
  api_keys:
    - id: demo
      hash: 264c5c83f07d30c7ccc1083d7e416705272eafa1fc15167e09bed57b7697faed
      scopes: [users:read]
upstreams:
%s
routes:
%s
`

func fullConfig(adminEnv, jwtEnv, upstreams, routes string) string {
	return fmt.Sprintf(fullConfigTemplate, adminEnv, jwtEnv, upstreams, routes)
}

func newHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func doRequest(t *testing.T, client *http.Client, method, url, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, raw)
	}
	return out
}

func errorCode(t *testing.T, raw []byte) string {
	t.Helper()
	body := decodeMap(t, raw)
	envelope, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %s", raw)
	}
	code, _ := envelope["code"].(string)
	return code
}

func mintJWT(t *testing.T, secret, subject string, scopes ...string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": subject,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if len(scopes) > 0 {
		claims["scope"] = strings.Join(scopes, " ")
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return token
}

type echoUpstream struct {
	name     string
	requests atomic.Int64
}

func newEchoUpstream(t *testing.T, name string) (*httptest.Server, *echoUpstream) {
	t.Helper()
	e := &echoUpstream{name: name}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upstream":   e.name,
			"method":     r.Method,
			"path":       r.URL.Path,
			"request_id": r.Header.Get(httpx.HeaderRequestID),
			"user_id":    r.Header.Get("X-User-Id"),
			"scopes":     r.Header.Get("X-User-Scopes"),
			"roles":      r.Header.Get("X-User-Roles"),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, e
}

func newTestHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, timeout time.Duration, description string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
