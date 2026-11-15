package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

func newMockServer(t *testing.T, rand func() float64) *httptest.Server {
	t.Helper()
	m := &mock{name: "users-1", rand: rand}
	srv := httptest.NewServer(requestID(m.routes()))
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, header http.Header) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("json: %v", err)
	}
	return resp, body
}

func TestHealthAndEcho(t *testing.T) {
	t.Parallel()
	srv := newMockServer(t, func() float64 { return 0.5 })

	resp, body := getJSON(t, srv.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "ok" || body["upstream"] != "users-1" {
		t.Fatalf("health = %d %v", resp.StatusCode, body)
	}

	header := http.Header{
		httpx.HeaderRequestID: []string{"req-123"},
		"X-User-Id":           []string{"user-42"},
		"X-User-Scopes":       []string{"users:read"},
		"Authorization":       []string{"Bearer must-not-leak"},
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/echo?a=1&b=2", strings.NewReader("secret body"))
	req.Header = header
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(httpx.HeaderRequestID) != "req-123" {
		t.Fatalf("echo = %d %q", resp.StatusCode, resp.Header.Get(httpx.HeaderRequestID))
	}
	if strings.Contains(string(raw), "must-not-leak") || strings.Contains(string(raw), "secret body") {
		t.Fatalf("echo leaked credentials or body: %s", raw)
	}
	var echo map[string]any
	if err := json.Unmarshal(raw, &echo); err != nil {
		t.Fatal(err)
	}
	if echo["method"] != "POST" || echo["path"] != "/api/echo" || echo["query"] != "a=1&b=2" ||
		echo["upstream"] != "users-1" || echo["request_id"] != "req-123" {
		t.Fatalf("echo = %v", echo)
	}
	identity, ok := echo["identity"].(map[string]any)
	if !ok || identity["X-User-Id"] != "user-42" || identity["X-User-Scopes"] != "users:read" {
		t.Fatalf("identity = %v", echo["identity"])
	}
}

func TestRequestIDIsGeneratedWhenInvalid(t *testing.T) {
	t.Parallel()
	srv := newMockServer(t, func() float64 { return 0.5 })
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set(httpx.HeaderRequestID, "bad id with spaces")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	id := resp.Header.Get(httpx.HeaderRequestID)
	if id == "" || id == "bad id with spaces" || len(id) != 26 {
		t.Fatalf("request id = %q", id)
	}
}

func TestDebugStatus(t *testing.T) {
	t.Parallel()
	srv := newMockServer(t, func() float64 { return 0.5 })
	resp, body := getJSON(t, srv.URL+"/debug/status?code=418", nil)
	if resp.StatusCode != http.StatusTeapot || body["status"].(float64) != 418 {
		t.Fatalf("status = %d %v", resp.StatusCode, body)
	}
	resp, body = getJSON(t, srv.URL+"/debug/status?code=99", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status = %d %v", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/debug/status?code=204", nil)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("204 endpoint = %d", resp.StatusCode)
	}
}

func TestDebugSlowHonoursContext(t *testing.T) {
	t.Parallel()
	srv := newMockServer(t, func() float64 { return 0.5 })

	begin := time.Now()
	_, body := getJSON(t, srv.URL+"/debug/slow?ms=20", nil)
	if elapsed := time.Since(begin); elapsed < 15*time.Millisecond {
		t.Fatalf("slow returned after %v", elapsed)
	}
	if body["delayed_ms"].(float64) != 20 {
		t.Fatalf("body = %v", body)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/debug/slow?ms=5000", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	begin = time.Now()
	_, err := (&http.Client{}).Do(req)
	if err == nil {
		t.Fatal("slow request ignored cancellation")
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestDebugFlaky(t *testing.T) {
	t.Parallel()
	srv := newMockServer(t, func() float64 { return 0.1 })
	resp, body := getJSON(t, srv.URL+"/debug/flaky?rate=1", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("flaky = %d %v", resp.StatusCode, body)
	}
	resp, body = getJSON(t, srv.URL+"/debug/flaky?rate=0", nil)
	if resp.StatusCode != http.StatusOK || body["upstream"] != "users-1" {
		t.Fatalf("not flaky = %d %v", resp.StatusCode, body)
	}
	resp, body = getJSON(t, srv.URL+"/debug/flaky?rate=nope", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rate = %d %v", resp.StatusCode, body)
	}
}

func TestSafeNameAndLogger(t *testing.T) {
	t.Parallel()
	if got := safeName("  "); got != defaultMockTag {
		t.Fatalf("blank name = %q", got)
	}
	if got := safeName("a\x00b\n"); got != "ab" {
		t.Fatalf("sanitized name = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := safeName(long); len(got) != 128 {
		t.Fatalf("long name length = %d", len(got))
	}
	var buf bytes.Buffer
	mockLogger(&buf, "debug").Debug("visible")
	if !strings.Contains(buf.String(), "visible") {
		t.Fatalf("log = %q", buf.String())
	}
	buf.Reset()
	mockLogger(&buf, "error").Info("hidden")
	if buf.Len() != 0 {
		t.Fatalf("log = %q", buf.String())
	}
	mockLogger(nil, "").Info("discarded")
}

func TestRunFlagErrors(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if code := run([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	errOut.Reset()
	if code := run([]string{"extra"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
}
