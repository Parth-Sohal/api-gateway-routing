package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestCatalog(t *testing.T) {
	t.Parallel()
	m := New("v1.2.3")
	// Vectors do not appear in the exposition until a label set is touched.
	m.ObserveHTTP("r", "GET", 200, time.Millisecond, 1)
	m.HTTPInflight(0)
	m.Panic("r")
	m.UpstreamAttempt("u", "http://a", OutcomeSuccess, time.Millisecond)
	m.UpstreamInflight("u", "http://a", 0)
	m.Retry("u", "5xx")
	m.BreakerState("u", 0)
	m.BreakerTransition("u", "closed", "open", "test")
	m.RateLimit("r", DecisionAllowed)
	m.AuthFailure("r", "test")
	m.TargetHealthy("u", "http://a", true)
	m.Probe("u", "http://a", ProbeSuccess, time.Millisecond)
	m.ConfigReload(true, time.Millisecond, time.Now())
	m.ConnectionState("active", 0)
	body := render(t, m)
	for _, name := range []string{
		"gateway_build_info",
		"gateway_http_requests_total",
		"gateway_http_request_duration_seconds",
		"gateway_http_inflight_requests",
		"gateway_http_response_bytes_total",
		"gateway_http_panics_total",
		"gateway_upstream_requests_total",
		"gateway_upstream_duration_seconds",
		"gateway_upstream_inflight_requests",
		"gateway_upstream_retries_total",
		"gateway_breaker_state",
		"gateway_breaker_transitions_total",
		"gateway_ratelimit_decisions_total",
		"gateway_auth_failures_total",
		"gateway_target_healthy",
		"gateway_target_probes_total",
		"gateway_target_probe_duration_seconds",
		"gateway_config_reloads_total",
		"gateway_config_reload_duration_seconds",
		"gateway_config_last_reload_success_timestamp_seconds",
		"gateway_connections",
		"go_goroutines",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("missing metric %q", name)
		}
	}
	if !strings.Contains(body, `gateway_build_info{version="v1.2.3"} 1`) {
		t.Fatalf("build info missing:\n%s", body)
	}
}

func TestRecordEverything(t *testing.T) {
	t.Parallel()
	m := New("test")
	m.ObserveHTTP("api", "GET", http.StatusOK, time.Millisecond, 10)
	m.HTTPInflight(2)
	m.Panic("api")
	m.UpstreamAttempt("pool", "http://a", OutcomeSuccess, 5*time.Millisecond)
	m.UpstreamInflight("pool", "http://a", 1)
	m.Retry("pool", "5xx")
	m.BreakerState("pool", 2)
	m.BreakerTransition("pool", "closed", "open", "ratio")
	m.RateLimit("api", DecisionLimited)
	m.AuthFailure("api", "invalid_token")
	m.TargetHealthy("pool", "http://a", false)
	m.Probe("pool", "http://a", ProbeFailure, 2*time.Millisecond)
	m.ConfigReload(true, 3*time.Millisecond, time.Unix(1_700_000_000, 0))
	m.ConfigReload(false, time.Millisecond, time.Unix(1_800_000_000, 0))
	m.ConnectionState("active", 2)

	body := render(t, m)
	for _, want := range []string{
		`gateway_http_requests_total{method="GET",route="api",status="200"} 1`,
		`gateway_http_response_bytes_total{method="GET",route="api"} 10`,
		`gateway_http_request_duration_seconds_count{method="GET",route="api"} 1`,
		`gateway_http_inflight_requests 2`,
		`gateway_http_panics_total{route="api"} 1`,
		`gateway_upstream_requests_total{outcome="success",target="http://a",upstream="pool"} 1`,
		`gateway_upstream_duration_seconds_count{target="http://a",upstream="pool"} 1`,
		`gateway_upstream_inflight_requests{target="http://a",upstream="pool"} 1`,
		`gateway_upstream_retries_total{reason="5xx",upstream="pool"} 1`,
		`gateway_breaker_state{upstream="pool"} 2`,
		`gateway_breaker_transitions_total{from="closed",reason="ratio",to="open",upstream="pool"} 1`,
		`gateway_ratelimit_decisions_total{decision="limited",route="api"} 1`,
		`gateway_auth_failures_total{reason="invalid_token",route="api"} 1`,
		`gateway_target_healthy{target="http://a",upstream="pool"} 0`,
		`gateway_target_probes_total{result="failure",target="http://a",upstream="pool"} 1`,
		`gateway_target_probe_duration_seconds_count{target="http://a",upstream="pool"} 1`,
		`gateway_config_reloads_total{result="success"} 1`,
		`gateway_config_reloads_total{result="failure"} 1`,
		`gateway_config_last_reload_success_timestamp_seconds 1.7e+09`,
		`gateway_connections{state="active"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}
}

func TestObserveHTTPDefaults(t *testing.T) {
	t.Parallel()
	m := New("test")
	m.ObserveHTTP("", "", 0, 0, 0)
	body := render(t, m)
	if !strings.Contains(body, `gateway_http_requests_total{method="UNKNOWN",route="unmatched",status="0"} 1`) {
		t.Fatalf("body:\n%s", body)
	}
	if strings.Contains(body, "gateway_http_response_bytes_total") {
		t.Fatalf("zero-byte response created a counter:\n%s", body)
	}
}

func TestNilMetrics(t *testing.T) {
	t.Parallel()
	var m *Metrics
	m.ObserveHTTP("r", "GET", 200, time.Millisecond, 1)
	m.HTTPInflight(1)
	m.Panic("r")
	m.UpstreamAttempt("u", "t", OutcomeSuccess, time.Millisecond)
	m.UpstreamInflight("u", "t", 1)
	m.Retry("u", "x")
	m.BreakerState("u", 0)
	m.BreakerTransition("u", "a", "b", "c")
	m.RateLimit("r", DecisionAllowed)
	m.AuthFailure("r", "x")
	m.TargetHealthy("u", "t", true)
	m.Probe("u", "t", ProbeSuccess, time.Millisecond)
	m.ConfigReload(true, time.Millisecond, time.Now())
	m.ConnectionState("idle", 1)
	if m.Handler() == nil || m.Gatherer() == nil {
		t.Fatal("nil accessors returned nil")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil handler status = %d", rec.Code)
	}
}

func TestNewDefaultsVersion(t *testing.T) {
	t.Parallel()
	m := New("")
	body := render(t, m)
	if !strings.Contains(body, `gateway_build_info{version="dev"} 1`) {
		t.Fatalf("body:\n%s", body)
	}
	if _, err := m.Gatherer().Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
}
