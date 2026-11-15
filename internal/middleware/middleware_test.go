package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

func renderMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func newRequestIDTestHandler(t *testing.T, tp httpx.TrustedProxies) (http.Handler, **httpx.Info) {
	t.Helper()
	var seen *httpx.Info
	h := RequestID(tp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.InfoFrom(r.Context())
	}))
	return h, &seen
}

func TestRequestIDGenerates(t *testing.T) {
	t.Parallel()
	h, seen := newRequestIDTestHandler(t, httpx.TrustedProxies{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "192.0.2.10:4321"
	h.ServeHTTP(rec, req)

	id := rec.Header().Get(HeaderRequestID)
	if len(id) != 26 {
		t.Fatalf("request id %q has length %d", id, len(id))
	}
	info := *seen
	if info == nil || info.RequestID != id {
		t.Fatalf("info = %+v", info)
	}
	if info.ClientIP != "192.0.2.10" {
		t.Fatalf("client ip = %q", info.ClientIP)
	}
}

func TestRequestIDPreservesValid(t *testing.T) {
	t.Parallel()
	h, seen := newRequestIDTestHandler(t, httpx.TrustedProxies{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderRequestID, "client-req_123.abc")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(HeaderRequestID); got != "client-req_123.abc" {
		t.Fatalf("header = %q", got)
	}
	if (*seen).RequestID != "client-req_123.abc" {
		t.Fatalf("info = %+v", *seen)
	}
}

func TestRequestIDReplacesInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{"bad id", "line\nbreak", strings.Repeat("a", 129), "ünïcode"}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			h, _ := newRequestIDTestHandler(t, httpx.TrustedProxies{})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set(HeaderRequestID, in)
			h.ServeHTTP(rec, req)
			got := rec.Header().Get(HeaderRequestID)
			if got == in || len(got) != 26 {
				t.Fatalf("header = %q", got)
			}
		})
	}
}

func TestRequestIDTrustedProxy(t *testing.T) {
	t.Parallel()
	tp, err := httpx.ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	h, seen := newRequestIDTestHandler(t, tp)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.1.2.3:9999"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(rec, req)
	if (*seen).ClientIP != "203.0.113.9" {
		t.Fatalf("client ip = %q", (*seen).ClientIP)
	}

	h, seen = newRequestIDTestHandler(t, httpx.TrustedProxies{})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.1.2.3:9999"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(rec, req)
	if (*seen).ClientIP != "10.1.2.3" {
		t.Fatalf("untrusted client ip = %q", (*seen).ClientIP)
	}
}

func TestAccessLogAndMetrics(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	m := metrics.New("test")
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		info := httpx.InfoFrom(r.Context())
		info.Route = "api"
		info.Upstream = "api-pool"
		info.Target = "http://127.0.0.1:9001"
		info.Retries = 2
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}), RequestID(httpx.TrustedProxies{}), AccessLog(AccessConfig{
		Enabled: true, Logger: logger, Metrics: m,
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/items?api_key=super-secret", nil)
	req.RemoteAddr = "192.0.2.5:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != "hello" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no access log line")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log json: %v (%s)", err, line)
	}
	for key, want := range map[string]any{
		"msg":      "request",
		"status":   float64(http.StatusCreated),
		"bytes":    float64(5),
		"route":    "api",
		"method":   "GET",
		"path":     "/v1/items",
		"upstream": "api-pool",
		"target":   "http://127.0.0.1:9001",
		"retries":  float64(2),
	} {
		if got := entry[key]; got != want {
			t.Fatalf("%s = %v, want %v (line %s)", key, got, want, line)
		}
	}
	if entry["request_id"] == "" || entry["client_ip"] != "192.0.2.5" {
		t.Fatalf("missing request metadata: %v", entry)
	}
	if entry["duration_ms"].(float64) <= 0 {
		t.Fatalf("duration = %v", entry["duration_ms"])
	}
	if strings.Contains(line, "super-secret") {
		t.Fatalf("query string leaked into log: %s", line)
	}

	body := renderMetrics(t, m)
	for _, want := range []string{
		`gateway_http_requests_total{method="GET",route="api",status="201"} 1`,
		`gateway_http_response_bytes_total{method="GET",route="api"} 5`,
		`gateway_http_request_duration_seconds_count{method="GET",route="api"} 1`,
		`gateway_http_inflight_requests 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestAccessLogDisabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := AccessLog(AccessConfig{Enabled: false, Logger: logger})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if buf.Len() != 0 {
		t.Fatalf("log = %s", buf.String())
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAccessLogNormalizesImplicit200(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := AccessLog(AccessConfig{Enabled: true, Logger: logger})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["status"] != float64(200) || entry["route"] != "unmatched" {
		t.Fatalf("entry = %v", entry)
	}
}

func TestRecoveryWritesEnvelope(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	m := metrics.New("test")
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.InfoFrom(r.Context()).Route = "boom"
		panic("kaboom")
	}), RequestID(httpx.TrustedProxies{}), Recovery(logger, m))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Error.Code != httpx.CodeInternalError {
		t.Fatalf("code = %q", body.Error.Code)
	}
	if body.Error.RequestID == "" || body.Error.RequestID != rec.Header().Get(HeaderRequestID) {
		t.Fatalf("request id = %q header = %q", body.Error.RequestID, rec.Header().Get(HeaderRequestID))
	}

	line := strings.TrimSpace(buf.String())
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(entry["level"].(string), "error") || entry["msg"] != "panic recovered" || entry["panic"] != "kaboom" {
		t.Fatalf("log = %v", entry)
	}
	if !strings.Contains(entry["stack"].(string), "goroutine") {
		t.Fatal("stack missing")
	}
	if !strings.Contains(renderMetrics(t, m), `gateway_http_panics_total{route="boom"} 1`) {
		t.Fatal("panic metric missing")
	}
}

func TestRecoveryAfterResponseStarted(t *testing.T) {
	t.Parallel()
	h := Recovery(nil, metrics.New("test"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("late failure")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestRecoveryRepanicsAbortHandler(t *testing.T) {
	t.Parallel()
	h := Recovery(nil, metrics.New("test"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want ErrAbortHandler", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Fatal("handler did not panic")
}

func TestChainOrder(t *testing.T) {
	t.Parallel()
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-in")
				next.ServeHTTP(w, r)
				order = append(order, name+"-out")
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("a"), nil, mw("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	want := []string{"a-in", "b-in", "handler", "b-out", "a-out"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestLogSafe(t *testing.T) {
	t.Parallel()
	if got := logSafe("a\nb\x00c", 100); got != "abc" {
		t.Fatalf("logSafe = %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := logSafe(long, 64); len(got) != 64 {
		t.Fatalf("length = %d", len(got))
	}
	if got := logSafe("", 10); got != "" {
		t.Fatalf("empty = %q", got)
	}
}
