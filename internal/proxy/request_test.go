package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/balancer"
	"github.com/Parth-Sohal/api-gateway-routing/internal/breaker"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

func TestNewRetryPolicy(t *testing.T) {
	t.Parallel()
	p := newRetryPolicy(config.RetryConfig{})
	if !p.enabled || p.maxAttempts != 2 || !p.onConn || len(p.statuses) != 3 {
		t.Fatalf("default policy = %+v", p)
	}
	if !p.allowsMethod("get") || !p.allowsMethod("PUT") || p.allowsMethod("POST") {
		t.Fatalf("default methods wrong: %+v", p.methods)
	}
	if !p.retryStatus(502) || p.retryStatus(500) || p.retryStatus(429) {
		t.Fatalf("default statuses wrong: %+v", p.statuses)
	}
	if !p.retryError(false, false) || p.retryError(true, false) || p.retryError(false, true) {
		t.Fatalf("default error handling wrong: %+v", p)
	}

	p = newRetryPolicy(config.RetryConfig{
		Enabled:     boolPtr(false),
		MaxAttempts: 99,
		Methods:     []string{"post", " patch "},
		On:          []string{"timeout", "500", "599", "400", "bogus"},
		BaseBackoff: config.Duration{Duration: 10 * time.Millisecond},
		MaxBackoff:  config.Duration{Duration: 100 * time.Millisecond},
	})
	if p.enabled || p.allowsMethod("POST") {
		t.Fatalf("disabled policy = %+v", p)
	}

	p = newRetryPolicy(config.RetryConfig{
		Enabled:     boolPtr(true),
		MaxAttempts: 99,
		Methods:     []string{"post", " patch "},
		On:          []string{"timeout", "500", "599", "400", "bogus"},
		BaseBackoff: config.Duration{Duration: 10 * time.Millisecond},
		MaxBackoff:  config.Duration{Duration: 100 * time.Millisecond},
	})
	if p.maxAttempts != 10 {
		t.Fatalf("policy = %+v", p)
	}
	if !p.allowsMethod("POST") || !p.allowsMethod("PATCH") || p.allowsMethod("GET") {
		t.Fatalf("methods = %+v", p.methods)
	}
	if !p.onTimeout || p.onConn || !p.retryStatus(500) || !p.retryStatus(599) || p.retryStatus(400) {
		t.Fatalf("statuses = %+v onTimeout=%v onConn=%v", p.statuses, p.onTimeout, p.onConn)
	}
	if !p.retryError(true, false) || p.retryError(false, false) {
		t.Fatal("timeout-only policy retried connection errors")
	}
	if p.bodyBuffer != defaultBodyBuffer {
		t.Fatalf("body buffer = %d", p.bodyBuffer)
	}
}

func TestRetryBackoff(t *testing.T) {
	t.Parallel()
	p := retryPolicy{baseBackoff: 100 * time.Millisecond, maxBackoff: 500 * time.Millisecond}
	cases := map[int]time.Duration{
		0: 100 * time.Millisecond,
		1: 100 * time.Millisecond,
		2: 200 * time.Millisecond,
		3: 400 * time.Millisecond,
		4: 500 * time.Millisecond,
		5: 500 * time.Millisecond,
	}
	for attempt, want := range cases {
		if got := p.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
	if got := (retryPolicy{}).backoff(3); got != 0 {
		t.Fatalf("zero base backoff = %v", got)
	}
}

func TestRetryBudget(t *testing.T) {
	t.Parallel()
	if b := newRetryBudget(0); b.perRequest != 0.1 {
		t.Fatalf("default percent = %v", b.perRequest)
	}
	b := newRetryBudget(50)
	if b.perRequest != 0.5 || b.burst != 3 || b.tokens != 3 {
		t.Fatalf("budget = %+v", b)
	}
	if !b.allow() || !b.allow() || !b.allow() {
		t.Fatal("burst not available")
	}
	if b.allow() {
		t.Fatal("retry allowed with no tokens")
	}
	for i := 0; i < 10; i++ {
		b.deposit()
	}
	if b.tokens != b.burst {
		t.Fatalf("tokens = %v, want cap %v", b.tokens, b.burst)
	}
	var nilBudget *retryBudget
	nilBudget.deposit()
	if nilBudget.allow() {
		t.Fatal("nil budget allowed a retry")
	}
	if !newRetryBudget(1).allow() || !newRetryBudget(100).allow() {
		t.Fatal("extreme percentages should still allow a burst")
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep: %v", err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep = %v", err)
	}
}

func TestRequestBodyOpen(t *testing.T) {
	t.Parallel()
	var nilBody *requestBody
	rc, n, ok := nilBody.open()
	if rc != nil || n != 0 || !ok {
		t.Fatalf("nil body open = %v %d %v", rc, n, ok)
	}

	b := &requestBody{data: []byte("abc"), replayable: true}
	rc, n, ok = b.open()
	if !ok || n != 3 {
		t.Fatalf("open = %d %v", n, ok)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "abc" {
		t.Fatalf("data = %q", got)
	}
	rc2, _, ok := b.open()
	if !ok {
		t.Fatal("buffered data should replay")
	}
	got, _ = io.ReadAll(rc2)
	rc2.Close()
	if string(got) != "abc" {
		t.Fatalf("second read = %q", got)
	}

	stream := &requestBody{stream: io.NopCloser(strings.NewReader("x")), length: 1}
	if _, _, ok := stream.open(); !ok {
		t.Fatal("first stream open failed")
	}
	if _, _, ok := stream.open(); ok {
		t.Fatal("stream reused")
	}

	if gb := b.getBody(); gb == nil {
		t.Fatal("buffered body has no getBody")
	}
	if gb := stream.getBody(); gb != nil {
		t.Fatal("stream body should not expose getBody")
	}
}

func TestPrepareRequestBody(t *testing.T) {
	t.Parallel()
	policy := retryPolicy{bodyBuffer: 8}
	w := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodPut, "/x", http.NoBody)
	body, err := prepareRequestBody(w, req, policy, true, 100)
	if err != nil || !body.replayable {
		t.Fatalf("no body = %+v, %v", body, err)
	}

	req = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("12345"))
	body, err = prepareRequestBody(w, req, policy, true, 4)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversize known body = %v", err)
	}

	req = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("1234567890"))
	body, err = prepareRequestBody(w, req, policy, true, 100)
	if err != nil {
		t.Fatalf("large body: %v", err)
	}
	if body.replayable || body.stream == nil {
		t.Fatalf("large body should stream without replay: %+v", body)
	}
	if _, _, ok := body.open(); !ok {
		t.Fatal("stream body not open")
	}
	if _, _, ok := body.open(); ok {
		t.Fatal("stream body reused")
	}

	req = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("1234567890"))
	req.ContentLength = -1
	body, err = prepareRequestBody(w, req, policy, false, 4)
	if err != nil {
		t.Fatalf("streaming oversize: %v", err)
	}
	if body.replayable {
		t.Fatal("streaming body marked replayable")
	}
	_, _, _ = body.open()
	if _, err := io.ReadAll(body.stream); err == nil {
		t.Fatal("MaxBytesReader did not stop an oversize stream")
	}

	req = httptest.NewRequest(http.MethodPut, "/x", nil)
	body, err = prepareRequestBody(w, req, policy, false, 0)
	if err != nil || !body.replayable {
		t.Fatalf("nil body = %+v, %v", body, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestPrepareRequestBodyReadError(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPut, "/x", errReader{})
	req.Body = io.NopCloser(errReader{})
	req.ContentLength = 5
	body, err := prepareRequestBody(httptest.NewRecorder(), req, retryPolicy{bodyBuffer: 16}, true, 100)
	if body != nil || !errors.Is(err, errReadBody) {
		t.Fatalf("body = %+v, err = %v", body, err)
	}
}

func TestTargetURLAndPrefixes(t *testing.T) {
	t.Parallel()
	base := &url.URL{Scheme: "http", Host: "up.example", Path: "/base", RawQuery: "ignored"}
	req := httptest.NewRequest(http.MethodGet, "/api/items?q=2&q=3", nil)

	got := targetURL(base, req, "/api")
	if got.Path != "/base/items" || got.RawQuery != "q=2&q=3" || got.Scheme != "http" || got.Host != "up.example" {
		t.Fatalf("target = %s", got)
	}
	if base.Path != "/base" || base.RawQuery != "ignored" {
		t.Fatal("targetURL mutated the base URL")
	}

	got = targetURL(base, req, "")
	if got.Path != "/base/api/items" {
		t.Fatalf("unstripped path = %q", got.Path)
	}
}

func TestStripPath(t *testing.T) {
	t.Parallel()
	cases := []struct{ path, prefix, want string }{
		{"/api/items", "", "/api/items"},
		{"/api", "/api", "/"},
		{"/api/items", "/api", "/items"},
		{"/api/items", "/api/", "/items"},
		{"/apiv2/items", "/api", "/apiv2/items"},
		{"/other", "/api", "/other"},
	}
	for _, c := range cases {
		if got := stripPath(c.path, c.prefix); got != c.want {
			t.Errorf("stripPath(%q, %q) = %q, want %q", c.path, c.prefix, got, c.want)
		}
	}
}

func TestJoinPaths(t *testing.T) {
	t.Parallel()
	cases := []struct{ base, req, want string }{
		{"", "", "/"},
		{"", "items", "/items"},
		{"", "/items", "/items"},
		{"/base", "", "/base/"},
		{"/base/", "/items", "/base/items"},
		{"base", "items", "/base/items"},
		{"/base", "/", "/base/"},
	}
	for _, c := range cases {
		if got := joinPaths(c.base, c.req); got != c.want {
			t.Errorf("joinPaths(%q, %q) = %q, want %q", c.base, c.req, got, c.want)
		}
	}
}

func TestBuildUpstreamRequest(t *testing.T) {
	t.Parallel()
	target := &balancer.Target{ID: "http://up.example", URL: mustURL(t, "http://up.example/base")}
	route := &config.Route{Name: "r", StripPrefix: "/api", PreserveHost: true}
	req, info := testRequest(http.MethodPost, "http://gateway.example/api/x?q=1", strings.NewReader("body"))
	req.Header.Set("Connection", "X-Drop")
	req.Header.Set("X-Drop", "gone")
	req.Header.Set("Upgrade", "h2c")

	up, err := buildUpstreamRequest(context.Background(), req, route, target, io.NopCloser(strings.NewReader("body")), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL.String() != "http://up.example/base/x?q=1" {
		t.Fatalf("url = %s", up.URL)
	}
	if up.Host != "gateway.example" {
		t.Fatalf("host = %q", up.Host)
	}
	if up.Header.Get("X-Drop") != "" || up.Header.Get("Upgrade") != "" || up.Header.Get("Connection") != "" {
		t.Fatalf("hop-by-hop headers survived: %v", up.Header)
	}
	if up.Header.Get(httpx.HeaderRequestID) != info.RequestID {
		t.Fatalf("request id = %q", up.Header.Get(httpx.HeaderRequestID))
	}
	if up.Header.Get("Content-Length") != "" {
		t.Fatal("Content-Length header set explicitly")
	}
	if up.ContentLength != 4 || up.RequestURI != "" || up.GetBody != nil {
		t.Fatalf("request metadata = %+v", up)
	}
	if up.Header.Get("X-Forwarded-For") != "192.0.2.5" {
		t.Fatalf("xff = %q", up.Header.Get("X-Forwarded-For"))
	}
}

func TestBuildUpstreamRequestDefaultsAndHTTPS(t *testing.T) {
	t.Parallel()
	target := &balancer.Target{URL: mustURL(t, "http://up.example")}
	route := &config.Route{Name: "r"}
	req := httptest.NewRequest(http.MethodGet, "https://gateway.example/x", nil)
	req.TLS = &tlsState{}
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Proto", "https")

	up, err := buildUpstreamRequest(context.Background(), req, route, target, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.Host != "up.example" {
		t.Fatalf("host = %q", up.Host)
	}
	if got := up.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("proto = %q", got)
	}
	if got := up.Header.Get("X-Forwarded-For"); got == "1.2.3.4" || got == "" {
		t.Fatalf("untrusted xff not rewritten: %q", got)
	}
	if up.ContentLength != 0 {
		t.Fatalf("content length = %d", up.ContentLength)
	}
}

func TestCopyResponseHeaders(t *testing.T) {
	t.Parallel()
	src := http.Header{
		"X-Upstream":    {"a"},
		"Connection":    {"X-Drop"},
		"X-Drop":        {"bad"},
		"X-Request-Id":  {"spoofed"},
		"Cache-Control": {"no-store"},
	}
	dst := http.Header{}
	info := &httpx.Info{RequestID: "gateway-id"}
	copyResponseHeaders(dst, src, info)

	if dst.Get("X-Upstream") != "a" || dst.Get("Cache-Control") != "no-store" {
		t.Fatalf("dst = %v", dst)
	}
	if dst.Get("X-Drop") != "" || dst.Get("Connection") != "" {
		t.Fatalf("hop headers copied: %v", dst)
	}
	if dst.Get(httpx.HeaderRequestID) != "gateway-id" {
		t.Fatalf("request id = %q", dst.Get(httpx.HeaderRequestID))
	}
	if src.Get("X-Drop") != "bad" {
		t.Fatal("source headers mutated")
	}

	dst = http.Header{}
	copyResponseHeaders(dst, src, nil)
	if dst.Get(httpx.HeaderRequestID) != "" {
		t.Fatalf("spoofed id kept without info: %q", dst.Get(httpx.HeaderRequestID))
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()
	if !isTimeout(timeoutErr{}) || !isTimeout(context.DeadlineExceeded) {
		t.Fatal("timeout not detected")
	}
	if isTimeout(errors.New("plain")) {
		t.Fatal("plain error treated as timeout")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestStatusOutcomeAndBreakerOutcome(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{
		200: "success",
		302: "success",
		404: "client_error",
		500: "server_error",
	} {
		if got := statusOutcome(status); got != want {
			t.Errorf("statusOutcome(%d) = %q, want %q", status, got, want)
		}
	}
	if err := breakerOutcome(attemptResult{err: context.Canceled, canceled: true}, true); !errors.Is(err, breaker.ErrIgnored) {
		t.Fatalf("canceled outcome = %v", err)
	}
	if err := breakerOutcome(attemptResult{err: errors.New("dial")}, true); err == nil {
		t.Fatal("transport error ignored")
	}
	if err := breakerOutcome(attemptResult{}, true); err == nil {
		t.Fatal("nil response accepted")
	}
	if err := breakerOutcome(attemptResult{resp: &http.Response{}, status: 500}, false); !errors.Is(err, breaker.ErrIgnored) {
		t.Fatalf("500 outcome = %v", err)
	}
	if err := breakerOutcome(attemptResult{resp: &http.Response{}, status: 500}, true); err == nil {
		t.Fatal("countable 500 treated as success")
	}
	if err := breakerOutcome(attemptResult{resp: &http.Response{}, status: 404}, true); !errors.Is(err, breaker.ErrIgnored) {
		t.Fatalf("404 outcome = %v", err)
	}
	if err := breakerOutcome(attemptResult{resp: &http.Response{}, status: 204}, true); err != nil {
		t.Fatalf("204 outcome = %v", err)
	}
}

func TestShortError(t *testing.T) {
	t.Parallel()
	if shortError(nil) != "" {
		t.Fatal("nil error not empty")
	}
	ue := &url.Error{Op: "Get", URL: "http://x/?api_key=secret", Err: errors.New("dial refused")}
	if got := shortError(ue); got != "dial refused" {
		t.Fatalf("shortError = %q", got)
	}
	long := strings.Repeat("e", 500)
	if got := shortError(errors.New(long)); len(got) != 200 {
		t.Fatalf("length = %d", len(got))
	}
}

type tlsState = tls.ConnectionState
