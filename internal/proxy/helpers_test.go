package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

type harness struct {
	proxy   *Proxy
	metrics *metrics.Metrics
	clock   *clock.Fake
}

func newHarness(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	m := metrics.New("test")
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	p, err := Build(cfg, Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
		Clock:   c,
		Sleep:   func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(p.Close)
	return &harness{proxy: p, metrics: m, clock: c}
}

func boolPtr(v bool) *bool { return &v }

func testUpstream(name string, targets ...string) config.Upstream {
	ts := make([]config.Target, len(targets))
	for i, u := range targets {
		ts[i] = config.Target{URL: u, Weight: 1}
	}
	return config.Upstream{
		Name:     name,
		Targets:  ts,
		Balancer: "round_robin",
		HealthCheck: config.HealthCheck{
			HealthyThreshold:   2,
			UnhealthyThreshold: 3,
			ExpectedStatusMin:  200,
			ExpectedStatusMax:  399,
		},
		Breaker: config.BreakerConfig{Enabled: boolPtr(false)},
		Retry:   config.RetryConfig{Enabled: boolPtr(false)},
	}
}

func enableRetry(u *config.Upstream, max int, methods, on []string, buffer int) {
	u.Retry = config.RetryConfig{
		Enabled:         boolPtr(true),
		MaxAttempts:     max,
		On:              on,
		Methods:         methods,
		BodyBufferBytes: buffer,
		BudgetPercent:   100,
	}
}

func enableBreaker(u *config.Upstream, failureThreshold int, count500 bool, openTimeout time.Duration) {
	u.Breaker = config.BreakerConfig{
		Enabled:           boolPtr(true),
		FailureThreshold:  failureThreshold,
		FailureRatio:      1,
		MinRequests:       1,
		Window:            config.Duration{Duration: 10 * time.Second},
		Buckets:           10,
		OpenTimeout:       config.Duration{Duration: openTimeout},
		HalfOpenMaxProbes: 1,
		HalfOpenSuccesses: 1,
		Count500:          count500,
	}
}

func testRoute(upstream string) *config.Route {
	return &config.Route{
		Name:         "r",
		Upstream:     upstream,
		Timeout:      config.Duration{Duration: 2 * time.Second},
		MaxBodyBytes: 1 << 20,
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func testRequest(method, target string, body io.Reader) (*http.Request, *httpx.Info) {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "192.0.2.5:1234"
	req.Host = "gateway.example"
	info := &httpx.Info{RequestID: "req-1", ClientIP: "192.0.2.5", Route: "r"}
	req = req.WithContext(httpx.WithInfo(req.Context(), info))
	return req, info
}

func renderMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := jsonUnmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// jsonUnmarshal is a small indirection keeping imports tidy across test files.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
