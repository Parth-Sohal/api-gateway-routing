// Package admin serves the private operations listener: liveness/readiness,
// the JSON admin API, the embedded dashboard and pprof. Every endpoint except
// the probes and the static dashboard page requires the admin bearer token;
// the token is compared in constant time and never logged.
package admin

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"sort"
	"strings"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/proxy"
)

const reloadTimeout = 30 * time.Second

//go:embed dashboard.html
var dashboardHTML []byte

// Source is the runtime state the admin API inspects and reloads. The server
// implements it; admin tests use a fake.
type Source interface {
	AdminSnapshot() Snapshot
	Reload(ctx context.Context) error
	Ready() (bool, string)
}

// Snapshot is a point-in-time view of the serving configuration and upstream
// runtime state. It is built by the server state that owns those components.
type Snapshot struct {
	Config          *config.Config
	Upstreams       []proxy.UpstreamSnapshot
	RateLimiter     string
	Reloads         uint64
	LastReload      time.Time
	LastReloadError string
}

// Deps are the explicit dependencies of the admin listener.
type Deps struct {
	Logger    *slog.Logger
	Metrics   *metrics.Metrics
	Clock     clock.Clock
	Source    Source
	Token     string
	Version   string
	StartedAt time.Time
}

// Handler is the admin HTTP handler. It is safe for concurrent use.
type Handler struct {
	logger    *slog.Logger
	metrics   *metrics.Metrics
	source    Source
	token     []byte
	version   string
	startedAt time.Time
	clock     clock.Clock
	mux       *http.ServeMux
	dashboard []byte
}

// New builds the admin handler. An empty token or nil source is a programming
// error: the admin listener must fail closed rather than serve unauthenticated
// state or panic on the first request.
func New(deps Deps) (*Handler, error) {
	if strings.TrimSpace(deps.Token) == "" {
		return nil, errors.New("admin: token is required")
	}
	if deps.Source == nil {
		return nil, errors.New("admin: source is required")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	c := deps.Clock
	if c == nil {
		c = clock.Real{}
	}
	started := deps.StartedAt
	if started.IsZero() {
		started = c.Now()
	}
	version := deps.Version
	if version == "" {
		version = "dev"
	}

	h := &Handler{
		logger:    logger,
		metrics:   deps.Metrics,
		source:    deps.Source,
		token:     []byte(deps.Token),
		version:   version,
		startedAt: started,
		clock:     c,
		mux:       http.NewServeMux(),
		dashboard: dashboardHTML,
	}
	h.routes()
	return h, nil
}

func (h *Handler) routes() {
	h.mux.HandleFunc("/healthz", h.handleHealth)
	h.mux.HandleFunc("/readyz", h.handleReady)
	h.mux.HandleFunc("/dashboard", h.handleDashboard)
	h.mux.HandleFunc("/dashboard/", h.handleDashboard)
	h.mux.HandleFunc("/", h.handleRoot)

	h.mux.Handle("/admin/api/overview", h.auth(http.HandlerFunc(h.handleOverview)))
	h.mux.Handle("/admin/api/upstreams", h.auth(http.HandlerFunc(h.handleUpstreams)))
	h.mux.Handle("/admin/api/config", h.auth(http.HandlerFunc(h.handleConfig)))
	h.mux.Handle("/admin/reload", h.auth(http.HandlerFunc(h.handleReload)))

	h.mux.Handle("/debug/pprof/", h.auth(http.HandlerFunc(pprof.Index)))
	h.mux.Handle("/debug/pprof/cmdline", h.auth(http.HandlerFunc(pprof.Cmdline)))
	h.mux.Handle("/debug/pprof/profile", h.auth(http.HandlerFunc(pprof.Profile)))
	h.mux.Handle("/debug/pprof/symbol", h.auth(http.HandlerFunc(pprof.Symbol)))
	h.mux.Handle("/debug/pprof/trace", h.auth(http.HandlerFunc(pprof.Trace)))
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gateway-admin"`)
			h.writeError(w, r, http.StatusUnauthorized, httpx.CodeUnauthorized, "Admin token required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	hdr := r.Header.Get("Authorization")
	if len(hdr) <= len(prefix) || !strings.EqualFold(hdr[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(hdr[len(prefix):])
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), h.token) == 1
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	ready, reason := h.source.Ready()
	status := http.StatusOK
	body := map[string]any{"status": "ready", "ready": true}
	if !ready {
		status = http.StatusServiceUnavailable
		body["status"] = "not_ready"
		body["ready"] = false
		if reason != "" {
			body["reason"] = reason
		}
	}
	h.writeJSON(w, status, body)
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; "+
			"frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(h.dashboard)
}

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		h.writeError(w, r, http.StatusNotFound, httpx.CodeRouteNotFound, "Not found.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusTemporaryRedirect)
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	snap := h.source.AdminSnapshot()
	ready, reason := h.source.Ready()
	h.writeJSON(w, http.StatusOK, h.buildOverview(snap, ready, reason))
}

func (h *Handler) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	snap := h.source.AdminSnapshot()
	h.writeJSON(w, http.StatusOK, map[string]any{
		"upstreams": upstreamViews(snap.Config, snap.Upstreams),
		"count":     len(snap.Upstreams),
	})
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.methodNotAllowed(w, http.MethodGet)
		return
	}
	snap := h.source.AdminSnapshot()
	if snap.Config == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, httpx.CodeInternalError, "Configuration unavailable.")
		return
	}
	body, err := config.RedactedYAML(snap.Config)
	if err != nil {
		h.logger.Error("render redacted config", slog.String("error", err.Error()))
		h.writeError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "Configuration unavailable.")
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *Handler) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.methodNotAllowed(w, http.MethodPost)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), reloadTimeout)
	defer cancel()
	if err := h.source.Reload(ctx); err != nil {
		h.logger.Error("admin reload failed", slog.String("error", err.Error()))
		h.writeError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "Reload failed; the previous configuration remains active.")
		return
	}
	snap := h.source.AdminSnapshot()
	body := map[string]any{"status": "reloaded", "reloads": snap.Reloads}
	if !snap.LastReload.IsZero() {
		body["last_reload"] = snap.LastReload.UTC().Format(time.RFC3339Nano)
	}
	h.writeJSON(w, http.StatusOK, body)
}

func (h *Handler) buildOverview(snap Snapshot, ready bool, reason string) overview {
	up := h.clock.Now().Sub(h.startedAt)
	if up < 0 {
		up = 0
	}
	view := overview{
		Version:         h.version,
		UptimeSeconds:   up.Seconds(),
		StartedAt:       h.startedAt.UTC().Format(time.RFC3339Nano),
		Ready:           ready,
		ReadinessReason: reason,
		RateLimiter:     snap.RateLimiter,
		Reloads:         snap.Reloads,
		LastReloadError: snap.LastReloadError,
		Routes:          routeViews(snap.Config),
		Upstreams:       upstreamViews(snap.Config, snap.Upstreams),
	}
	if !snap.LastReload.IsZero() {
		ts := snap.LastReload.UTC().Format(time.RFC3339Nano)
		view.LastReload = &ts
	}
	return view
}

type overview struct {
	Version         string         `json:"version"`
	UptimeSeconds   float64        `json:"uptime_seconds"`
	StartedAt       string         `json:"started_at"`
	Ready           bool           `json:"ready"`
	ReadinessReason string         `json:"readiness_reason,omitempty"`
	RateLimiter     string         `json:"rate_limiter,omitempty"`
	Reloads         uint64         `json:"reloads"`
	LastReload      *string        `json:"last_reload,omitempty"`
	LastReloadError string         `json:"last_reload_error,omitempty"`
	Routes          []routeView    `json:"routes"`
	Upstreams       []upstreamView `json:"upstreams"`
}

type routeView struct {
	Name         string         `json:"name"`
	Host         string         `json:"host,omitempty"`
	PathPrefix   string         `json:"path_prefix"`
	Methods      []string       `json:"methods,omitempty"`
	Upstream     string         `json:"upstream"`
	AuthRequired bool           `json:"auth_required"`
	Scopes       []string       `json:"scopes,omitempty"`
	Roles        []string       `json:"roles,omitempty"`
	RateLimit    *rateLimitView `json:"rate_limit,omitempty"`
	Timeout      string         `json:"timeout,omitempty"`
	MaxBodyBytes int64          `json:"max_body_bytes,omitempty"`
}

type rateLimitView struct {
	Capacity     int64   `json:"capacity"`
	RefillPerSec float64 `json:"refill_per_sec"`
	Key          string  `json:"key"`
	Header       string  `json:"header,omitempty"`
	FailOpen     bool    `json:"fail_open"`
}

type upstreamView struct {
	Name     string       `json:"name"`
	Balancer string       `json:"balancer"`
	Targets  []targetView `json:"targets"`
	Breaker  *breakerView `json:"breaker,omitempty"`
}

type targetView struct {
	URL                 string `json:"url"`
	Weight              int    `json:"weight"`
	Available           bool   `json:"available"`
	Inflight            int64  `json:"inflight"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
	LastChange          string `json:"last_change,omitempty"`
}

type breakerView struct {
	State               string  `json:"state"`
	FailureRatio        float64 `json:"failure_ratio"`
	WindowRequests      int64   `json:"window_requests"`
	WindowFailures      int64   `json:"window_failures"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	OpenedAt            string  `json:"opened_at,omitempty"`
}

func routeViews(cfg *config.Config) []routeView {
	if cfg == nil {
		return []routeView{}
	}
	out := make([]routeView, 0, len(cfg.Routes))
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		v := routeView{
			Name:         r.Name,
			Host:         r.Match.Host,
			PathPrefix:   r.Match.PathPrefix,
			Methods:      r.Match.Methods,
			Upstream:     r.Upstream,
			AuthRequired: r.Auth.Required,
			Scopes:       r.Auth.Scopes,
			Roles:        r.Auth.Roles,
			MaxBodyBytes: r.MaxBodyBytes,
		}
		if r.Timeout.Duration > 0 {
			v.Timeout = r.Timeout.Duration.String()
		}
		if r.RateLimit != nil {
			v.RateLimit = &rateLimitView{
				Capacity:     r.RateLimit.Capacity,
				RefillPerSec: r.RateLimit.RefillPerSec,
				Key:          r.RateLimit.Key,
				Header:       r.RateLimit.Header,
				FailOpen:     r.RateLimit.FailOpenOr(cfg.Redis.FailOpenEnabled()),
			}
		}
		out = append(out, v)
	}
	return out
}

func upstreamViews(cfg *config.Config, snapshots []proxy.UpstreamSnapshot) []upstreamView {
	out := make([]upstreamView, 0, len(snapshots))
	for i := range snapshots {
		s := &snapshots[i]
		v := upstreamView{
			Name:     s.Name,
			Balancer: balancerName(cfg, s.Name),
			Targets:  targetViews(s),
		}
		if s.Breaker != nil {
			b := &breakerView{
				State:               s.Breaker.State.String(),
				FailureRatio:        round(s.Breaker.FailureRatio),
				WindowRequests:      s.Breaker.WindowRequests,
				WindowFailures:      s.Breaker.WindowFailures,
				ConsecutiveFailures: s.Breaker.ConsecutiveFailures,
			}
			if !s.Breaker.OpenedAt.IsZero() {
				b.OpenedAt = s.Breaker.OpenedAt.UTC().Format(time.RFC3339Nano)
			}
			v.Breaker = b
		}
		out = append(out, v)
	}
	return out
}

func targetViews(s *proxy.UpstreamSnapshot) []targetView {
	health := make(map[string]int, len(s.Health))
	for i := range s.Health {
		st := &s.Health[i]
		health[st.Target] = i
	}
	out := make([]targetView, 0, len(s.Targets))
	for _, t := range s.Targets {
		v := targetView{
			URL:       t.ID,
			Weight:    t.Weight,
			Available: t.Available,
			Inflight:  t.Inflight,
		}
		if idx, ok := health[t.ID]; ok {
			st := &s.Health[idx]
			v.ConsecutiveFailures = st.ConsecutiveFailures
			v.LastError = st.LastError
			if !st.LastChange.IsZero() {
				v.LastChange = st.LastChange.UTC().Format(time.RFC3339Nano)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func balancerName(cfg *config.Config, upstream string) string {
	if cfg == nil {
		return ""
	}
	if u, ok := cfg.UpstreamByName(upstream); ok {
		return u.Balancer
	}
	return ""
}

func round(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

func (h *Handler) methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	h.writeError(w, nil, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "Method not allowed.")
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if r == nil {
		r = &http.Request{Header: http.Header{}}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, r, httpx.NewError(status, code, message))
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.logger.Error("encode admin response", slog.String("error", err.Error()))
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, &http.Request{Header: http.Header{}},
			httpx.NewError(http.StatusInternalServerError, httpx.CodeInternalError, "Internal server error."))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// Ready is a small helper for tests and for callers that want the readiness
// decision without an HTTP round trip.
func (h *Handler) Ready() (bool, string) { return h.source.Ready() }

// Version returns the build version exposed by the admin API.
func (h *Handler) Version() string { return h.version }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
