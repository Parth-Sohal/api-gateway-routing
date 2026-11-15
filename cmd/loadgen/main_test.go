package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunText(t *testing.T) {
	t.Parallel()
	var (
		requests int64
		bad      int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if r.Method != http.MethodPost || r.Header.Get("X-Test") != "yes" ||
			string(body) != "hello" || r.URL.RawQuery != "x=1" {
			atomic.AddInt64(&bad, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"-url", srv.URL + "/echo?x=1",
		"-method", "post",
		"-requests", "60",
		"-concurrency", "6",
		"-H", "X-Test: yes",
		"-body", "hello",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if got := atomic.LoadInt64(&requests); got != 60 {
		t.Fatalf("server saw %d requests, want 60", got)
	}
	if got := atomic.LoadInt64(&bad); got != 0 {
		t.Fatalf("%d requests did not carry method/header/body/query", got)
	}
	text := out.String()
	for _, want := range []string{
		"POST " + srv.URL + "/echo?redacted",
		"requests      60 completed",
		"status codes  200=60",
		"result        success=60 failures=0 errors=0 timeouts=0",
		"latency       p50=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "?x=1") {
		t.Fatalf("query value leaked into output:\n%s", text)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunJSONAndFailOnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{
		"-url", srv.URL + "/ok", "-requests", "25", "-concurrency", "5", "-json",
	}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	var summary summaryJSON
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if summary.Requests != 25 || summary.Success != 25 || summary.Failures != 0 || summary.Errors != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.StatusCodes["201"] != 25 || summary.RequestsPerSecond <= 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.LatencyMS.P50 < 0 || summary.LatencyMS.Max < summary.LatencyMS.P50 {
		t.Fatalf("latency = %+v", summary.LatencyMS)
	}
	if summary.Interrupted {
		t.Fatalf("clean run marked interrupted")
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{
		"-url", srv.URL + "/fail", "-requests", "10", "-concurrency", "2", "-json", "-fail-on-error",
	}, &out, &errOut); code != 1 {
		t.Fatalf("fail-on-error exit = %d, stderr = %q", code, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json: %v", err)
	}
	if summary.Failures != 10 || summary.StatusCodes["500"] != 10 {
		t.Fatalf("summary = %+v", summary)
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{
		"-url", srv.URL + "/fail", "-requests", "5", "-concurrency", "1",
	}, &out, &errOut); code != 0 {
		t.Fatalf("without fail-on-error exit = %d, stderr = %q", code, errOut.String())
	}
}

func TestRunTransportErrors(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{
		"-url", "http://" + addr + "/", "-requests", "4", "-concurrency", "2", "-timeout", "500ms",
	}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "errors=4") {
		t.Fatalf("output = %s", text)
	}
	if !strings.Contains(text, "status codes  none") {
		t.Fatalf("output = %s", text)
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{
		"-url", "http://" + addr + "/", "-requests", "2", "-concurrency", "1", "-fail-on-error",
	}, &out, &errOut); code != 1 {
		t.Fatalf("fail-on-error exit = %d, stderr = %q", code, errOut.String())
	}
}

func TestRunDuration(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	begin := time.Now()
	if code := run(context.Background(), []string{
		"-url", srv.URL, "-duration", "150ms", "-concurrency", "4", "-json",
	}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if elapsed := time.Since(begin); elapsed < 140*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("duration run took %v", elapsed)
	}
	var summary summaryJSON
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json: %v", err)
	}
	if summary.Requests == 0 || summary.Success != summary.Requests {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Interrupted {
		t.Fatalf("duration timeout must not be reported as interrupted")
	}
	if summary.ConfiguredDuration != 150 {
		t.Fatalf("configured duration = %v", summary.ConfiguredDuration)
	}
}

func TestRunCanceledByParent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	begin := time.Now()
	if code := run(ctx, []string{"-url", srv.URL, "-concurrency", "4", "-json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("canceled run took %v", elapsed)
	}
	var summary summaryJSON
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !summary.Interrupted {
		t.Fatalf("canceled run not marked interrupted: %+v", summary)
	}
}

func TestRunRate(t *testing.T) {
	t.Parallel()
	var requests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	begin := time.Now()
	if code := run(context.Background(), []string{
		"-url", srv.URL, "-requests", "10", "-rate", "200", "-concurrency", "4", "-json",
	}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	elapsed := time.Since(begin)
	if elapsed < 35*time.Millisecond {
		t.Fatalf("rate-limited run finished too quickly: %v", elapsed)
	}
	if got := atomic.LoadInt64(&requests); got != 10 {
		t.Fatalf("server saw %d requests", got)
	}
	var summary summaryJSON
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("json: %v", err)
	}
	if summary.ConfiguredRate != 200 || summary.Requests != 10 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRunFlagAndValidationErrors(t *testing.T) {
	t.Parallel()
	base := []string{"-url", "http://127.0.0.1:1/", "-requests", "1"}
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"-nope"}},
		{"extra arg", append(append([]string{}, base...), "leftover")},
		{"missing url", []string{"-requests", "1"}},
		{"bad scheme", []string{"-url", "ftp://example.com", "-requests", "1"}},
		{"no host", []string{"-url", "http:///path", "-requests", "1"}},
		{"zero concurrency", append(append([]string{}, base...), "-concurrency", "0")},
		{"excess concurrency", append(append([]string{}, base...), "-concurrency", "5000")},
		{"negative requests", []string{"-url", "http://127.0.0.1:1/", "-requests", "-1"}},
		{"negative duration", append(append([]string{}, base...), "-duration", "-1s")},
		{"huge duration", append(append([]string{}, base...), "-duration", "2h")},
		{"negative rate", append(append([]string{}, base...), "-rate", "-2")},
		{"huge rate", append(append([]string{}, base...), "-rate", "2000000")},
		{"tiny timeout", append(append([]string{}, base...), "-timeout", "1ns")},
		{"huge timeout", append(append([]string{}, base...), "-timeout", "10m")},
		{"bad header", append(append([]string{}, base...), "-H", "no-colon")},
		{"blank header name", append(append([]string{}, base...), "-H", ": value")},
		{"header injection", append(append([]string{}, base...), "-H", "X-Test: a\r\nb")},
		{"blank method", append(append([]string{}, base...), "-method", "  ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(context.Background(), tc.args, &out, &errOut); code != 2 {
				t.Fatalf("run(%v) = %d, want 2 (stderr %q)", tc.args, code, errOut.String())
			}
			if errOut.Len() == 0 {
				t.Fatalf("run(%v) wrote no diagnostic", tc.args)
			}
		})
	}
}

func TestHeaderFlags(t *testing.T) {
	t.Parallel()
	var flags headerFlags
	if err := flags.Set("X-One: 1"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("X-One: 2"); err != nil {
		t.Fatal(err)
	}
	headers, err := flags.header()
	if err != nil {
		t.Fatal(err)
	}
	if got := headers.Values("X-One"); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("headers = %v", headers)
	}
	if flags.String() != "X-One: 1, X-One: 2" {
		t.Fatalf("String() = %q", flags.String())
	}

	empty := headerFlags{}
	if got, err := empty.header(); got != nil || err != nil {
		t.Fatalf("empty header() = %v, %v", got, err)
	}

	many := headerFlags{}
	for i := 0; i < maxHeaderCount; i++ {
		if err := many.Set(fmt.Sprintf("X-%d: v", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := many.Set("X-Overflow: v"); err == nil {
		t.Fatal("header limit not enforced")
	}
	if err := empty.Set(strings.Repeat("X", maxHeaderName+1) + ": v"); err == nil {
		t.Fatal("header name length not enforced")
	}
	if err := empty.Set("X-Long: " + strings.Repeat("v", maxHeaderValue+1)); err == nil {
		t.Fatal("header value length not enforced")
	}
}

func TestHistogram(t *testing.T) {
	t.Parallel()
	var h histogram
	if h.percentile(0.5) != 0 || h.avg() != 0 {
		t.Fatalf("empty histogram = %v/%v", h.percentile(0.5), h.avg())
	}
	for _, d := range []time.Duration{
		500 * time.Microsecond,
		2 * time.Millisecond,
		100 * time.Millisecond,
		2 * time.Minute,
	} {
		h.observe(d)
	}
	if h.count != 4 || h.max != 2*time.Minute {
		t.Fatalf("histogram = %+v", h)
	}
	if got := h.percentile(0.25); got != 500*time.Microsecond {
		t.Fatalf("p25 = %v", got)
	}
	if got := h.percentile(0.5); got != 2500*time.Microsecond {
		t.Fatalf("p50 = %v", got)
	}
	if got := h.percentile(0.75); got != 100*time.Millisecond {
		t.Fatalf("p75 = %v", got)
	}
	if got := h.percentile(1); got != 2*time.Minute {
		t.Fatalf("p100 = %v", got)
	}
	if got := h.avg(); got != (500*time.Microsecond+2*time.Millisecond+100*time.Millisecond+2*time.Minute)/4 {
		t.Fatalf("avg = %v", got)
	}

	var other histogram
	for i := 0; i < 100; i++ {
		other.observe(5 * time.Millisecond)
	}
	h.merge(&other)
	if h.count != 104 || h.percentile(0.5) != 5*time.Millisecond {
		t.Fatalf("merged histogram = count %d p50 %v", h.count, h.percentile(0.5))
	}
}

func TestStatsMerge(t *testing.T) {
	t.Parallel()
	a, b := newStats(), newStats()
	a.observe(200, time.Millisecond)
	a.observe(500, 2*time.Millisecond)
	b.observe(302, 3*time.Millisecond)
	b.observe(99, time.Millisecond)
	b.errs = 2
	b.timeouts = 1
	a.merge(b)
	if a.completed != 4 || a.success != 2 || a.failures != 1 || a.otherStatus != 1 || a.errs != 2 || a.timeouts != 1 {
		t.Fatalf("stats = %+v", a)
	}
	if a.status[200] != 1 || a.status[500] != 1 || a.status[302] != 1 {
		t.Fatalf("status = %v", a.status)
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("https://user:pass@example.com:8443/api?token=secret#frag")
	if err != nil {
		t.Fatal(err)
	}
	got := redactURL(u)
	if strings.Contains(got, "user") || strings.Contains(got, "pass") || strings.Contains(got, "secret") || strings.Contains(got, "frag") {
		t.Fatalf("redactURL = %q", got)
	}
	if got != "https://example.com:8443/api?redacted" {
		t.Fatalf("redactURL = %q", got)
	}
	plain, err := url.Parse("http://example.com/path")
	if err != nil {
		t.Fatal(err)
	}
	if got := redactURL(plain); got != "http://example.com/path" {
		t.Fatalf("redactURL plain = %q", got)
	}
}

func TestRateIntervalAndBoundedInt(t *testing.T) {
	t.Parallel()
	if got := rateInterval(0); got != 0 {
		t.Fatalf("rateInterval(0) = %v", got)
	}
	if got := rateInterval(1000); got != time.Millisecond {
		t.Fatalf("rateInterval(1000) = %v", got)
	}
	if got := rateInterval(1e9); got != time.Microsecond {
		t.Fatalf("rateInterval(1e9) = %v", got)
	}
	if got := boundedInt(0, 1, 10); got != 1 {
		t.Fatalf("boundedInt low = %d", got)
	}
	if got := boundedInt(20, 1, 10); got != 10 {
		t.Fatalf("boundedInt high = %d", got)
	}
	if got := boundedInt(5, 1, 10); got != 5 {
		t.Fatalf("boundedInt mid = %d", got)
	}
}
