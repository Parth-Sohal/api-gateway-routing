// Package metrics owns the Prometheus registry and every operator-visible
// series the gateway exports. A Metrics value is created once per process and
// passed explicitly to the components that record into it; there are no
// package globals and no implicit default registerer.
//
// Label values are bounded by configuration: route names, upstream names,
// target URLs, a small status-code set and fixed decision enums. Nothing
// request-specific (raw paths, user IDs, headers) is used as a label.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcome label values for upstream attempts.
const (
	OutcomeSuccess        = "success"
	OutcomeClientError    = "client_error"
	OutcomeServerError    = "server_error"
	OutcomeTransportError = "transport_error"
	OutcomeTimeout        = "timeout"
	OutcomeCanceled       = "canceled"
)

// Rate-limit decision label values.
const (
	DecisionAllowed  = "allowed"
	DecisionLimited  = "limited"
	DecisionError    = "error"
	DecisionFailOpen = "failopen"
)

// Probe result label values.
const (
	ProbeSuccess = "success"
	ProbeFailure = "failure"
)

var upstreamBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

var probeBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// Metrics is the gateway's metric set.
type Metrics struct {
	registry *prometheus.Registry
	handler  http.Handler

	buildInfo *prometheus.GaugeVec

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInflight prometheus.Gauge
	httpBytes    *prometheus.CounterVec
	panics       *prometheus.CounterVec

	upstreamRequests *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	upstreamInflight *prometheus.GaugeVec
	retries          *prometheus.CounterVec

	breakerState       *prometheus.GaugeVec
	breakerTransitions *prometheus.CounterVec

	rateLimitDecisions *prometheus.CounterVec
	authFailures       *prometheus.CounterVec

	targetHealthy *prometheus.GaugeVec
	probes        *prometheus.CounterVec
	probeDuration *prometheus.HistogramVec

	configReloads    *prometheus.CounterVec
	configReloadTime prometheus.Histogram
	lastReload       prometheus.Gauge

	connections *prometheus.GaugeVec
}

// New builds the metric set and registers Go/process collectors on a private
// registry.
func New(version string) *Metrics {
	if version == "" {
		version = "dev"
	}
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gateway", Name: "build_info",
			Help: "Build information; always 1.",
		}, []string{"version"}),

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests by route, method and response status.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gateway", Subsystem: "http", Name: "request_duration_seconds",
			Help: "End-to-end HTTP request latency.", Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		httpInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gateway", Subsystem: "http", Name: "inflight_requests",
			Help: "Requests currently being served.",
		}),
		httpBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "http", Name: "response_bytes_total",
			Help: "Response bytes written to clients.",
		}, []string{"route", "method"}),
		panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "http", Name: "panics_total",
			Help: "Recovered handler panics.",
		}, []string{"route"}),

		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "upstream", Name: "requests_total",
			Help: "Upstream attempts by upstream, target and outcome.",
		}, []string{"upstream", "target", "outcome"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gateway", Subsystem: "upstream", Name: "duration_seconds",
			Help: "Upstream attempt latency.", Buckets: upstreamBuckets,
		}, []string{"upstream", "target"}),
		upstreamInflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gateway", Subsystem: "upstream", Name: "inflight_requests",
			Help: "Upstream attempts currently in flight.",
		}, []string{"upstream", "target"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "upstream", Name: "retries_total",
			Help: "Retries by upstream and reason.",
		}, []string{"upstream", "reason"}),

		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gateway", Subsystem: "breaker", Name: "state",
			Help: "Breaker state: 0 closed, 1 half-open, 2 open.",
		}, []string{"upstream"}),
		breakerTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "breaker", Name: "transitions_total",
			Help: "Breaker state transitions.",
		}, []string{"upstream", "from", "to", "reason"}),

		rateLimitDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "ratelimit", Name: "decisions_total",
			Help: "Rate limit decisions by route and outcome.",
		}, []string{"route", "decision"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "auth", Name: "failures_total",
			Help: "Authentication and authorization failures by route and reason.",
		}, []string{"route", "reason"}),

		targetHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gateway", Subsystem: "target", Name: "healthy",
			Help: "Target health: 1 healthy, 0 ejected.",
		}, []string{"upstream", "target"}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "target", Name: "probes_total",
			Help: "Active health probes by result.",
		}, []string{"upstream", "target", "result"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gateway", Subsystem: "target", Name: "probe_duration_seconds",
			Help: "Active health probe latency.", Buckets: probeBuckets,
		}, []string{"upstream", "target"}),

		configReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway", Subsystem: "config", Name: "reloads_total",
			Help: "Configuration reload attempts by result.",
		}, []string{"result"}),
		configReloadTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "gateway", Subsystem: "config", Name: "reload_duration_seconds",
			Help: "Configuration reload duration.", Buckets: upstreamBuckets,
		}),
		lastReload: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gateway", Subsystem: "config", Name: "last_reload_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful configuration reload.",
		}),

		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gateway", Name: "connections",
			Help: "Open client connections by state.",
		}, []string{"state"}),
	}

	reg.MustRegister(
		m.buildInfo,
		m.httpRequests, m.httpDuration, m.httpInflight, m.httpBytes, m.panics,
		m.upstreamRequests, m.upstreamDuration, m.upstreamInflight, m.retries,
		m.breakerState, m.breakerTransitions,
		m.rateLimitDecisions, m.authFailures,
		m.targetHealthy, m.probes, m.probeDuration,
		m.configReloads, m.configReloadTime, m.lastReload,
		m.connections,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m.buildInfo.WithLabelValues(version).Set(1)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m
}

// Handler serves the registry in Prometheus text/OpenMetrics format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return m.handler
}

// Gatherer exposes the registry for tests and admin diagnostics.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	if m == nil {
		return prometheus.NewRegistry()
	}
	return m.registry
}

// ObserveHTTP records one completed gateway request.
func (m *Metrics) ObserveHTTP(route, method string, status int, dur time.Duration, respBytes int64) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	if method == "" {
		method = "UNKNOWN"
	}
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(dur.Seconds())
	if respBytes > 0 {
		m.httpBytes.WithLabelValues(route, method).Add(float64(respBytes))
	}
}

// HTTPInflight tracks concurrently served requests.
func (m *Metrics) HTTPInflight(delta float64) {
	if m == nil {
		return
	}
	m.httpInflight.Add(delta)
}

// Panic records a recovered handler panic.
func (m *Metrics) Panic(route string) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	m.panics.WithLabelValues(route).Inc()
}

// UpstreamAttempt records one upstream attempt and its latency.
func (m *Metrics) UpstreamAttempt(upstream, target, outcome string, dur time.Duration) {
	if m == nil {
		return
	}
	m.upstreamRequests.WithLabelValues(upstream, target, outcome).Inc()
	m.upstreamDuration.WithLabelValues(upstream, target).Observe(dur.Seconds())
}

// UpstreamInflight tracks attempts in flight per target.
func (m *Metrics) UpstreamInflight(upstream, target string, delta float64) {
	if m == nil {
		return
	}
	m.upstreamInflight.WithLabelValues(upstream, target).Add(delta)
}

// Retry records a retry decision.
func (m *Metrics) Retry(upstream, reason string) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(upstream, reason).Inc()
}

// BreakerState sets the breaker gauge for an upstream.
func (m *Metrics) BreakerState(upstream string, state float64) {
	if m == nil {
		return
	}
	m.breakerState.WithLabelValues(upstream).Set(state)
}

// BreakerTransition records a state change.
func (m *Metrics) BreakerTransition(upstream, from, to, reason string) {
	if m == nil {
		return
	}
	m.breakerTransitions.WithLabelValues(upstream, from, to, reason).Inc()
}

// RateLimit records a limiter decision.
func (m *Metrics) RateLimit(route, decision string) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	m.rateLimitDecisions.WithLabelValues(route, decision).Inc()
}

// AuthFailure records an authentication or authorization rejection.
func (m *Metrics) AuthFailure(route, reason string) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	m.authFailures.WithLabelValues(route, reason).Inc()
}

// TargetHealthy sets target health.
func (m *Metrics) TargetHealthy(upstream, target string, healthy bool) {
	if m == nil {
		return
	}
	v := 0.0
	if healthy {
		v = 1
	}
	m.targetHealthy.WithLabelValues(upstream, target).Set(v)
}

// Probe records an active health probe.
func (m *Metrics) Probe(upstream, target, result string, dur time.Duration) {
	if m == nil {
		return
	}
	m.probes.WithLabelValues(upstream, target, result).Inc()
	m.probeDuration.WithLabelValues(upstream, target).Observe(dur.Seconds())
}

// ConfigReload records a reload attempt. On success it also advances the
// last-success timestamp.
func (m *Metrics) ConfigReload(success bool, dur time.Duration, at time.Time) {
	if m == nil {
		return
	}
	result := "failure"
	if success {
		result = "success"
		m.lastReload.Set(float64(at.Unix()))
	}
	m.configReloads.WithLabelValues(result).Inc()
	m.configReloadTime.Observe(dur.Seconds())
}

// ConnectionState adjusts the gauge for client connections in a state.
func (m *Metrics) ConnectionState(state string, delta float64) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(state).Add(delta)
}
