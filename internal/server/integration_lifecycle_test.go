package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestIntegrationGracefulShutdownDrainsInflight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "slow-ok")
	}))

	cfg := mustParse(t, minimalConfig(fmt.Sprintf(`
  - name: slow
    targets:
      - url: %s
`, upstream.URL), `
  - name: slow
    match:
      path_prefix: /
    upstream: slow
    timeout: 10s
`))
	s := startE2EServer(t, cfg, "")
	client := newHTTPClient(t)

	type result struct {
		status int
		body   string
		err    error
	}
	inflight := make(chan result, 1)
	go func() {
		resp, err := client.Get(s.Public + "/slow")
		if err != nil {
			inflight <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		inflight <- result{status: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request never started")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	// Readiness must drop immediately, before in-flight work finishes.
	waitFor(t, 3*time.Second, "readiness to drop", func() bool {
		ready, reason := s.Ready()
		return !ready && reason == "shutting down"
	})
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before the in-flight request drained: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-inflight:
		if got.err != nil {
			t.Fatalf("in-flight request failed: %v", got.err)
		}
		if got.status != http.StatusOK || got.body != "slow-ok" {
			t.Fatalf("in-flight response = %d %q", got.status, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done was not closed after shutdown")
	}

	// The listener is closed: new requests are refused.
	probe := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequest(http.MethodGet, s.Public+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := probe.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("new request succeeded after shutdown: %d", resp.StatusCode)
	}
}

func TestIntegrationMultiInstanceRateLimit(t *testing.T) {
	redis := miniredis.RunT(t)
	runMultiInstanceLimitTest(t, redis.Addr())
}

func TestIntegrationMultiInstanceRateLimitRealRedis(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("GATEWAY_TEST_REDIS"))
	if addr == "" {
		t.Skip("set GATEWAY_TEST_REDIS=host:port to run against a real Redis")
	}
	runMultiInstanceLimitTest(t, addr)
}

func runMultiInstanceLimitTest(t *testing.T, redisAddr string) {
	t.Helper()
	upstream, echo := newEchoUpstream(t, "shared")
	// A unique route name keeps repeated runs independent: the Redis key
	// namespace is rl:<route>:<key_type>:<identity>.
	route := fmt.Sprintf("limits-%d", time.Now().UnixNano())
	document := fmt.Sprintf(`
server:
  addr: "127.0.0.1:0"
  shutdown_grace: 5s
admin:
  enabled: false
metrics:
  enabled: false
redis:
  addr: %q
  op_timeout: 200ms
  fail_open: false
log:
  level: error
  format: text
  access_log: false
upstreams:
  - name: shared
    targets:
      - url: %s
routes:
  - name: %s
    match:
      path_prefix: /
    upstream: shared
    timeout: 5s
    rate_limit:
      capacity: 10
      refill_per_sec: 0.001
      key: ip
`, redisAddr, upstream.URL, route)

	first := startE2EServer(t, mustParse(t, document), "")
	second := startE2EServer(t, mustParse(t, document), "")
	client := newHTTPClient(t)

	var allowed, limited int
	for i := 0; i < 30; i++ {
		target := first.Public
		if i%2 == 1 {
			target = second.Public
		}
		resp, raw := doRequest(t, client, http.MethodGet, target+"/limited", "", nil)
		switch resp.StatusCode {
		case http.StatusOK:
			allowed++
			if got := resp.Header.Get("X-RateLimit-Limit"); got != "10" {
				t.Fatalf("limit header = %q", got)
			}
		case http.StatusTooManyRequests:
			limited++
			if code := errorCode(t, raw); code != "rate_limited" {
				t.Fatalf("limited error code = %q", code)
			}
			if resp.Header.Get("Retry-After") == "" {
				t.Fatal("429 response has no Retry-After")
			}
		default:
			t.Fatalf("unexpected status %d: %s", resp.StatusCode, raw)
		}
	}
	if allowed != 10 || limited != 20 {
		t.Fatalf("allowed=%d limited=%d, want 10/20 (shared bucket across instances)", allowed, limited)
	}
	if got := echo.requests.Load(); got != 10 {
		t.Fatalf("upstream saw %d allowed requests, want 10", got)
	}
}
