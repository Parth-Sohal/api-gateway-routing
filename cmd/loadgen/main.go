// Command loadgen is a small, dependency-free HTTP load generator used when
// k6 is unavailable (local development, CI smoke tests, chaos experiments).
//
// It runs a fixed number of requests or a fixed duration, reports status-code
// counts and latency percentiles, and keeps memory bounded: fixed-size latency
// buckets, a bounded status-code map, bounded request bodies and bodies are
// drained up to a fixed limit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxConcurrency   = 4096
	maxRequests      = 100_000_000
	maxDuration      = time.Hour
	maxTimeout       = 5 * time.Minute
	maxBodyBytes     = 1 << 20
	maxDrainBytes    = 4 << 20
	maxRate          = 1_000_000
	defaultDuration  = 10 * time.Second
	maxURLLength     = 8192
	maxHeaderName    = 256
	maxHeaderValue   = 8192
	maxHeaderCount   = 64
	maxMethodLength  = 32
	connectionIdle   = 90 * time.Second
	dialTimeout      = 5 * time.Second
	tlsTimeout       = 5 * time.Second
	expectContinue   = time.Second
	jsonIndentPrefix = ""
	jsonIndent       = "  "
)

// latencyBoundCount is the number of fixed histogram buckets (plus a +Inf
// overflow bucket). Percentiles report the upper bound of the bucket they land
// in, which is conservative and allocation-free.
const latencyBoundCount = 18

var latencyBounds = [latencyBoundCount]time.Duration{
	100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond,
	10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2500 * time.Millisecond, 5 * time.Second, 10 * time.Second,
	30 * time.Second, time.Minute,
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// headerFlags collects repeatable -H/header flags.
type headerFlags []string

func (h *headerFlags) String() string { return strings.Join(*h, ", ") }

func (h *headerFlags) Set(raw string) error {
	name, value, ok := strings.Cut(raw, ":")
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if !ok || name == "" {
		return fmt.Errorf("header %q must be \"Name: value\"", raw)
	}
	if len(name) > maxHeaderName || strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("invalid header name %q", name)
	}
	if len(value) > maxHeaderValue || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid value for header %q", name)
	}
	if len(*h) >= maxHeaderCount {
		return fmt.Errorf("at most %d headers are supported", maxHeaderCount)
	}
	*h = append(*h, name+": "+value)
	return nil
}

func (h *headerFlags) header() (http.Header, error) {
	if len(*h) == 0 {
		return nil, nil
	}
	out := make(http.Header, len(*h))
	for _, raw := range *h {
		name, value, _ := strings.Cut(raw, ":")
		out.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return out, nil
}

type config struct {
	target      *url.URL
	display     string
	method      string
	headers     http.Header
	body        []byte
	requests    int64
	duration    time.Duration
	concurrency int
	rate        float64
	timeout     time.Duration
	jsonOut     bool
	failOnError bool
}

func run(parent context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("url", "", "target URL (required)")
	method := fs.String("method", http.MethodGet, "HTTP method")
	requests := fs.Int64("requests", 0, "number of requests (0 = until -duration elapses)")
	duration := fs.Duration("duration", 0, "run duration (default 10s when -requests is 0; caps -requests runs too)")
	concurrency := fs.Int("concurrency", 10, fmt.Sprintf("concurrent workers (1-%d)", maxConcurrency))
	rate := fs.Float64("rate", 0, fmt.Sprintf("request rate per second, 0 = unlimited (max %d)", maxRate))
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	body := fs.String("body", "", "request body (at most 1 MiB)")
	jsonOut := fs.Bool("json", false, "print a JSON summary instead of text")
	failOnError := fs.Bool("fail-on-error", false, "exit 1 when any request fails (transport error or status >= 400)")
	var headers headerFlags
	fs.Var(&headers, "H", "request header \"Name: value\" (repeatable)")
	fs.Var(&headers, "header", "alias for -H")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "loadgen: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := validate(*target, *method, *requests, *duration, *concurrency, *rate, *timeout, *body, &headers)
	if err != nil {
		fmt.Fprintf(stderr, "loadgen: %v\n", err)
		return 2
	}
	cfg.jsonOut = *jsonOut
	cfg.failOnError = *failOnError

	summary, err := execute(parent, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "loadgen: %v\n", err)
		return 1
	}
	if err := writeSummary(stdout, summary, cfg); err != nil {
		fmt.Fprintf(stderr, "loadgen: write summary: %v\n", err)
		return 1
	}
	if cfg.failOnError && (summary.Errors > 0 || summary.Failures > 0) {
		return 1
	}
	return 0
}

func validate(target, method string, requests int64, duration time.Duration, concurrency int,
	rate float64, timeout time.Duration, body string, headers *headerFlags) (*config, error) {
	raw := strings.TrimSpace(target)
	if raw == "" {
		return nil, errors.New("-url is required")
	}
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("-url is longer than %d bytes", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse -url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("-url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("-url must include a host")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || len(method) > maxMethodLength {
		return nil, errors.New("-method must be 1-32 characters")
	}
	if requests < 0 || requests > maxRequests {
		return nil, fmt.Errorf("-requests must be between 0 and %d", maxRequests)
	}
	if duration < 0 || duration > maxDuration {
		return nil, fmt.Errorf("-duration must be between 0 and %s", maxDuration)
	}
	if requests == 0 && duration == 0 {
		duration = defaultDuration
	}
	if concurrency < 1 || concurrency > maxConcurrency {
		return nil, fmt.Errorf("-concurrency must be between 1 and %d", maxConcurrency)
	}
	if rate < 0 || rate > maxRate {
		return nil, fmt.Errorf("-rate must be between 0 and %d", maxRate)
	}
	if timeout < time.Millisecond || timeout > maxTimeout {
		return nil, fmt.Errorf("-timeout must be between 1ms and %s", maxTimeout)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("-body is longer than %d bytes", maxBodyBytes)
	}
	parsed, err := headers.header()
	if err != nil {
		return nil, err
	}
	return &config{
		target:      u,
		display:     redactURL(u),
		method:      method,
		headers:     parsed,
		body:        []byte(body),
		requests:    requests,
		duration:    duration,
		concurrency: concurrency,
		rate:        rate,
		timeout:     timeout,
	}, nil
}

// redactURL strips userinfo and query values so credentials in a URL never
// reach the terminal or CI logs.
func redactURL(u *url.URL) string {
	clone := *u
	clone.User = nil
	if clone.RawQuery != "" {
		clone.RawQuery = "redacted"
	}
	clone.Fragment = ""
	return clone.String()
}

type stats struct {
	completed   int64
	success     int64
	failures    int64
	errs        int64
	timeouts    int64
	otherStatus int64
	status      map[int]int64
	latency     histogram
}

func newStats() *stats {
	return &stats{status: make(map[int]int64, 8)}
}

func (s *stats) observe(status int, elapsed time.Duration) {
	s.completed++
	switch {
	case status >= 200 && status < 400:
		s.success++
	case status >= 400:
		s.failures++
	}
	if status >= 100 && status <= 599 {
		s.status[status]++
	} else {
		s.otherStatus++
	}
	s.latency.observe(elapsed)
}

func (s *stats) merge(other *stats) {
	s.completed += other.completed
	s.success += other.success
	s.failures += other.failures
	s.errs += other.errs
	s.timeouts += other.timeouts
	s.otherStatus += other.otherStatus
	for code, n := range other.status {
		s.status[code] += n
	}
	s.latency.merge(&other.latency)
}

type histogram struct {
	buckets [latencyBoundCount + 1]int64 // last bucket is > max bound
	count   int64
	sum     time.Duration
	max     time.Duration
}

func (h *histogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	h.count++
	h.sum += d
	if d > h.max {
		h.max = d
	}
	for i, bound := range latencyBounds {
		if d <= bound {
			h.buckets[i]++
			return
		}
	}
	h.buckets[len(latencyBounds)]++
}

func (h *histogram) merge(other *histogram) {
	for i, n := range other.buckets {
		h.buckets[i] += n
	}
	h.count += other.count
	h.sum += other.sum
	if other.max > h.max {
		h.max = other.max
	}
}

// percentile returns the upper bound of the bucket containing the p-th
// observation. p is in (0,1].
func (h *histogram) percentile(p float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	target := int64(math.Ceil(p * float64(h.count)))
	if target < 1 {
		target = 1
	}
	var seen int64
	for i, n := range h.buckets {
		seen += n
		if seen >= target {
			if i < len(latencyBounds) {
				return latencyBounds[i]
			}
			return h.max
		}
	}
	return h.max
}

func (h *histogram) avg() time.Duration {
	if h.count == 0 {
		return 0
	}
	return h.sum / time.Duration(h.count)
}

type latencyJSON struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

type summaryJSON struct {
	Target             string           `json:"target"`
	Method             string           `json:"method"`
	Concurrency        int              `json:"concurrency"`
	Requests           int64            `json:"requests"`
	DurationMS         float64          `json:"duration_ms"`
	RequestsPerSecond  float64          `json:"requests_per_second"`
	Success            int64            `json:"success"`
	Failures           int64            `json:"failures"`
	Errors             int64            `json:"errors"`
	Timeouts           int64            `json:"timeouts"`
	OtherStatus        int64            `json:"other_status,omitempty"`
	StatusCodes        map[string]int64 `json:"status_codes,omitempty"`
	LatencyMS          latencyJSON      `json:"latency_ms"`
	Interrupted        bool             `json:"interrupted,omitempty"`
	ConfiguredRate     float64          `json:"configured_rate,omitempty"`
	ConfiguredDuration float64          `json:"configured_duration_ms,omitempty"`
}

func execute(ctx context.Context, cfg *config) (*summaryJSON, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          boundedInt(cfg.concurrency*2, 2, 8192),
		MaxIdleConnsPerHost:   cfg.concurrency,
		IdleConnTimeout:       connectionIdle,
		TLSHandshakeTimeout:   tlsTimeout,
		ExpectContinueTimeout: expectContinue,
		DisableCompression:    true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	runCtx := ctx
	var cancel context.CancelFunc
	if cfg.duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cfg.duration)
		defer cancel()
	}

	l := &loader{cfg: cfg, client: client, interval: rateInterval(cfg.rate), drain: maxDrainBytes}
	jobs := make(chan struct{}, cfg.concurrency)
	aggregate := newStats()
	start := time.Now()

	var wg sync.WaitGroup
	results := make([]*stats, cfg.concurrency)
	for i := 0; i < cfg.concurrency; i++ {
		local := newStats()
		results[i] = local
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.worker(runCtx, jobs, local)
		}()
	}
	l.dispatch(runCtx, jobs)
	wg.Wait()
	elapsed := time.Since(start)

	for _, local := range results {
		aggregate.merge(local)
	}
	summary := buildSummary(cfg, aggregate, elapsed, ctx.Err() != nil)
	return summary, nil
}

func boundedInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func rateInterval(rate float64) time.Duration {
	if rate <= 0 {
		return 0
	}
	interval := time.Duration(float64(time.Second) / rate)
	if interval < time.Microsecond {
		interval = time.Microsecond
	}
	return interval
}

type loader struct {
	cfg      *config
	client   *http.Client
	interval time.Duration
	drain    int64
}

// dispatch feeds the worker pool. It returns the number of jobs it queued.
func (l *loader) dispatch(ctx context.Context, jobs chan<- struct{}) int64 {
	defer close(jobs)
	var timer *time.Timer
	if l.interval > 0 {
		timer = time.NewTimer(time.Hour)
		if !timer.Stop() {
			<-timer.C
		}
		defer timer.Stop()
	}
	var queued int64
	next := time.Now()
	for l.cfg.requests == 0 || queued < l.cfg.requests {
		if l.interval > 0 {
			if wait := time.Until(next); wait > 0 {
				timer.Reset(wait)
				select {
				case <-ctx.Done():
					return queued
				case <-timer.C:
				}
			}
			next = next.Add(l.interval)
			if lag := time.Since(next); lag > l.interval {
				next = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return queued
		case jobs <- struct{}{}:
			queued++
		}
	}
	return queued
}

func (l *loader) worker(ctx context.Context, jobs <-chan struct{}, out *stats) {
	for range jobs {
		if ctx.Err() != nil {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, l.cfg.timeout)
		req, err := l.newRequest(reqCtx)
		if err != nil {
			cancel()
			out.errs++
			continue
		}
		begin := time.Now()
		resp, err := l.client.Do(req)
		if err != nil {
			cancel()
			// A stopped run cancels in-flight requests; those are not failures.
			if ctx.Err() != nil {
				return
			}
			out.errs++
			if errors.Is(err, context.DeadlineExceeded) {
				out.timeouts++
			}
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, l.drain))
		_ = resp.Body.Close()
		elapsed := time.Since(begin)
		cancel()
		out.observe(resp.StatusCode, elapsed)
	}
}

func (l *loader) newRequest(ctx context.Context) (*http.Request, error) {
	var body io.Reader
	if len(l.cfg.body) > 0 {
		body = bytes.NewReader(l.cfg.body)
	}
	req, err := http.NewRequestWithContext(ctx, l.cfg.method, l.cfg.target.String(), body)
	if err != nil {
		return nil, err
	}
	for name, values := range l.cfg.headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return req, nil
}

func buildSummary(cfg *config, s *stats, elapsed time.Duration, interrupted bool) *summaryJSON {
	codes := make(map[string]int64, len(s.status))
	for code, n := range s.status {
		codes[strconv.Itoa(code)] = n
	}
	statuses := make([]int, 0, len(codes))
	for raw := range codes {
		n, _ := strconv.Atoi(raw)
		statuses = append(statuses, n)
	}
	sort.Ints(statuses)
	ordered := make(map[string]int64, len(codes))
	for _, code := range statuses {
		ordered[strconv.Itoa(code)] = codes[strconv.Itoa(code)]
	}
	seconds := elapsed.Seconds()
	rps := 0.0
	if seconds > 0 {
		rps = float64(s.completed) / seconds
	}
	return &summaryJSON{
		Target:            cfg.display,
		Method:            cfg.method,
		Concurrency:       cfg.concurrency,
		Requests:          s.completed,
		DurationMS:        round3(float64(elapsed.Microseconds()) / 1000),
		RequestsPerSecond: round3(rps),
		Success:           s.success,
		Failures:          s.failures,
		Errors:            s.errs,
		Timeouts:          s.timeouts,
		OtherStatus:       s.otherStatus,
		StatusCodes:       ordered,
		LatencyMS: latencyJSON{
			P50: round3(durationMS(s.latency.percentile(0.50))),
			P95: round3(durationMS(s.latency.percentile(0.95))),
			P99: round3(durationMS(s.latency.percentile(0.99))),
			Max: round3(durationMS(s.latency.max)),
			Avg: round3(durationMS(s.latency.avg())),
		},
		Interrupted:        interrupted,
		ConfiguredRate:     cfg.rate,
		ConfiguredDuration: round3(durationMS(cfg.duration)),
	}
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func writeSummary(w io.Writer, summary *summaryJSON, cfg *config) error {
	if cfg.jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent(jsonIndentPrefix, jsonIndent)
		return enc.Encode(summary)
	}
	statusLine := "status codes  none"
	if len(summary.StatusCodes) > 0 {
		codes := make([]int, 0, len(summary.StatusCodes))
		for raw := range summary.StatusCodes {
			code, err := strconv.Atoi(raw)
			if err == nil {
				codes = append(codes, code)
			}
		}
		sort.Ints(codes)
		parts := make([]string, 0, len(codes))
		for _, code := range codes {
			parts = append(parts, fmt.Sprintf("%d=%d", code, summary.StatusCodes[strconv.Itoa(code)]))
		}
		statusLine = "status codes  " + strings.Join(parts, " ")
	}
	interrupted := ""
	if summary.Interrupted {
		interrupted = " (interrupted)"
	}
	_, err := fmt.Fprintf(w, `target        %s %s
requests      %d%s completed in %.3fs (%.1f req/s)
concurrency   %d
%s
result        success=%d failures=%d errors=%d timeouts=%d
latency       p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms avg=%.3fms
`,
		summary.Method, summary.Target,
		summary.Requests, interrupted, summary.DurationMS/1000, summary.RequestsPerSecond,
		summary.Concurrency,
		statusLine,
		summary.Success, summary.Failures, summary.Errors, summary.Timeouts,
		summary.LatencyMS.P50, summary.LatencyMS.P95, summary.LatencyMS.P99,
		summary.LatencyMS.Max, summary.LatencyMS.Avg)
	return err
}
