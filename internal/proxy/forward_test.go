package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/balancer"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func flakyServer(t *testing.T, failures int, status int, bodyText string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= int64(failures) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "retry me")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, bodyText)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestRetryOnStatusThenSuccess(t *testing.T) {
	t.Parallel()
	srv, hits := flakyServer(t, 1, http.StatusServiceUnavailable, "eventual")
	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	enableRetry(&u, 3, nil, nil, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusOK || rec.Body.String() != "eventual" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if hits.Load() != 2 || info.Retries != 1 {
		t.Fatalf("hits = %d, retries = %d", hits.Load(), info.Retries)
	}
	body := renderMetrics(t, h.metrics)
	if !strings.Contains(body, `gateway_upstream_retries_total{reason="503",upstream="pool"} 1`) {
		t.Fatalf("retry metric missing:\n%s", body)
	}
}

func TestRetryOnConnectionError(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	live, hits := flakyServer(t, 0, http.StatusOK, "alive")

	u := testUpstream("pool", deadURL, live.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	enableRetry(&u, 3, nil, nil, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusOK || rec.Body.String() != "alive" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 || info.Retries != 1 {
		t.Fatalf("hits = %d, retries = %d", hits.Load(), info.Retries)
	}
}

func TestRetryReplaysBufferedBody(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies []string
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	enableRetry(&u, 3, []string{"PUT"}, []string{"502"}, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodPut, "http://gateway.example/x", strings.NewReader("replay-payload"))
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if info.Retries != 1 || hits.Load() != 2 {
		t.Fatalf("retries = %d, hits = %d", info.Retries, hits.Load())
	}
	if len(bodies) != 2 || bodies[0] != "replay-payload" || bodies[1] != "replay-payload" {
		t.Fatalf("bodies = %q", bodies)
	}
}

func TestNoRetryForNonIdempotentMethod(t *testing.T) {
	t.Parallel()
	srv, hits := flakyServer(t, 10, http.StatusServiceUnavailable, "never")
	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	enableRetry(&u, 3, nil, nil, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodPost, "http://gateway.example/x", strings.NewReader("once"))
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "retry me" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 || info.Retries != 0 {
		t.Fatalf("hits = %d, retries = %d", hits.Load(), info.Retries)
	}
}

func TestLargeBodyIsNotReplayed(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies []string
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	enableRetry(&u, 3, []string{"PUT"}, []string{"503"}, 4)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	payload := strings.Repeat("x", 64)
	req, info := testRequest(http.MethodPut, "http://gateway.example/x", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = %d", rec.Code)
	}
	if hits.Load() != 1 || info.Retries != 0 {
		t.Fatalf("large body retried: hits = %d, retries = %d", hits.Load(), info.Retries)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != payload {
		t.Fatalf("upstream body length = %d", len(strings.Join(bodies, "")))
	}
}

func TestKnownBodyTooLarge(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	enableRetry(&u, 3, nil, nil, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})
	route := testRoute("pool")
	route.MaxBodyBytes = 4

	req, _ := testRequest(http.MethodPut, "http://gateway.example/x", strings.NewReader("12345"))
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, route)

	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != httpx.CodePayloadTooLarge {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("oversize body reached upstream %d times", hits.Load())
	}
}

func TestUnknownLengthOversizeStream(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 10
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})
	route := testRoute("pool")
	route.MaxBodyBytes = 4

	req, _ := testRequest(http.MethodPut, "http://gateway.example/x", strings.NewReader("1234567890"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, route)

	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != httpx.CodePayloadTooLarge {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBodyReadFailureIsBadRequest(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	enableRetry(&u, 3, []string{"PUT"}, nil, 1024)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, _ := testRequest(http.MethodPut, "http://gateway.example/x", nil)
	req.Body = io.NopCloser(errReader{})
	req.ContentLength = 5
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != httpx.CodeInvalidRequest {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}
}

func TestRetryBudgetExhausted(t *testing.T) {
	t.Parallel()
	srv, hits := flakyServer(t, 100, http.StatusServiceUnavailable, "never")
	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	enableRetry(&u, 10, nil, nil, 1024)
	u.Retry.BudgetPercent = 100
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "retry me" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if info.Retries != 3 || hits.Load() != 4 {
		t.Fatalf("retries = %d, hits = %d", info.Retries, hits.Load())
	}
}

func TestRetryStopsWhenContextCancelled(t *testing.T) {
	t.Parallel()
	srv, _ := flakyServer(t, 100, http.StatusServiceUnavailable, "never")
	u := testUpstream("pool", srv.URL)
	u.HealthCheck.UnhealthyThreshold = 100
	enableRetry(&u, 5, nil, nil, 1024)
	u.Retry.BaseBackoff = config.Duration{Duration: time.Hour}
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})

	// The injected sleep returns nil without waiting, so bound the loop with a
	// short route timeout and verify it terminates.
	route := testRoute("pool")
	route.Timeout = config.Duration{Duration: 50 * time.Millisecond}
	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	begin := time.Now()
	h.proxy.Forward(rec, req, route)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestForwardUpstreamResponseHeadersAnd404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"missing"}`)
	}))
	defer srv.Close()

	u := testUpstream("pool", srv.URL)
	h := newHarness(t, &config.Config{Upstreams: []config.Upstream{u}})
	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	rec := httptest.NewRecorder()
	h.proxy.Forward(rec, req, testRoute("pool"))

	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"missing"}` {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Retry-After") != "7" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", "http://127.0.0.1:1")}}
	h := newHarness(t, cfg)
	up, _ := h.proxy.Upstream("pool")

	cases := []struct {
		name       string
		err        error
		timedOut   bool
		wantStatus int
		wantCode   string
	}{
		{"no targets", balancer.ErrNoTargets, false, http.StatusServiceUnavailable, httpx.CodeNoHealthyTargets},
		{"body too large", errBodyTooLarge, false, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge},
		{"read body", errReadBody, false, http.StatusBadRequest, httpx.CodeInvalidRequest},
		{"timeout", errors.New("slow"), true, http.StatusGatewayTimeout, httpx.CodeGatewayTimeout},
		{"connection", errors.New("dial"), false, http.StatusBadGateway, httpx.CodeBadGateway},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req, info := testRequest(http.MethodGet, "http://gateway.example/x", nil)
			rec := httptest.NewRecorder()
			h.proxy.failAttempt(rec, req, up, attemptResult{err: c.err, timedOut: c.timedOut})
			if rec.Code != c.wantStatus || errorCode(t, rec) != c.wantCode {
				t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
			}
			if info.ErrorCode != c.wantCode {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestCanceledRequestWritesNothing(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Upstreams: []config.Upstream{testUpstream("pool", "http://127.0.0.1:1")}}
	h := newHarness(t, cfg)
	up, _ := h.proxy.Upstream("pool")

	req, _ := testRequest(http.MethodGet, "http://gateway.example/x", nil)
	req = req.WithContext(cancelledContext())
	rec := httptest.NewRecorder()
	h.proxy.failAttempt(rec, req, up, attemptResult{err: errors.New("canceled"), canceled: true})
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("canceled request wrote %d %q", rec.Code, rec.Body.String())
	}
}
