package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/auth"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/middleware"
	"github.com/Parth-Sohal/api-gateway-routing/internal/proxy"
	"github.com/Parth-Sohal/api-gateway-routing/internal/ratelimit"
	"github.com/Parth-Sohal/api-gateway-routing/internal/router"
)

const (
	stateCloseWait = 5 * time.Second
	maxIdentityLen = 256
	maxScopeLen    = 2048
)

// state is one immutable serving snapshot: a validated config, compiled route
// table, authenticator, limiter wiring and proxy runtimes. Requests load one
// state and use it for their whole lifetime; a reload builds a new state and
// swaps it atomically.
type state struct {
	cfg         *config.Config
	logger      *slog.Logger
	metrics     *metrics.Metrics
	router      *router.Router
	auth        *auth.Authenticator
	proxy       *proxy.Proxy
	limiter     ratelimit.Limiter
	limiterMode string
	trusted     httpx.TrustedProxies
	sem         chan struct{}
	handler     http.Handler

	ctx        context.Context
	cancel     context.CancelFunc
	checksDone chan struct{}
	closeOnce  sync.Once
}

type stateDeps struct {
	Logger      *slog.Logger
	Metrics     *metrics.Metrics
	Limiter     ratelimit.Limiter
	LimiterMode string
}

func newState(cfg *config.Config, deps stateDeps) (*state, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	rt, err := router.New(cfg.Routes)
	if err != nil {
		return nil, err
	}
	authenticator, err := auth.New(cfg.Auth)
	if err != nil {
		return nil, err
	}
	trusted, err := httpx.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	px, err := proxy.Build(cfg, proxy.Deps{Logger: logger, Metrics: deps.Metrics})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &state{
		cfg:         cfg,
		logger:      logger,
		metrics:     deps.Metrics,
		router:      rt,
		auth:        authenticator,
		proxy:       px,
		limiter:     deps.Limiter,
		limiterMode: deps.LimiterMode,
		trusted:     trusted,
		ctx:         ctx,
		cancel:      cancel,
		checksDone:  make(chan struct{}),
	}
	if n := cfg.Server.MaxConcurrentRequests; n > 0 {
		s.sem = make(chan struct{}, n)
	}
	s.handler = middleware.Chain(
		http.HandlerFunc(s.serve),
		middleware.RequestID(trusted),
		middleware.AccessLog(middleware.AccessConfig{
			Enabled: cfg.Log.AccessLogEnabled(),
			Logger:  logger,
			Metrics: deps.Metrics,
		}),
		middleware.Recovery(logger, deps.Metrics),
	)

	go func() {
		defer close(s.checksDone)
		px.RunHealthChecks(ctx)
	}()
	return s, nil
}

// Close stops active health checks and releases idle upstream connections.
// In-flight requests keep their own connections; CloseIdleConnections only
// affects pooled idle sockets.
func (s *state) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		select {
		case <-s.checksDone:
		case <-time.After(stateCloseWait):
			s.logger.Warn("health checks did not stop within grace period")
		}
		s.proxy.Close()
	})
}

func (s *state) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *state) serve(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(w, r) {
		return
	}
	defer s.release()

	path, err := httpx.NormalizePath(r.URL.Path)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, httpx.CodeInvalidRequest, "Invalid request path.", 0)
		return
	}
	if path != r.URL.Path {
		r.URL.Path = path
		r.URL.RawPath = ""
	}

	matched, matchErr := s.router.Match(r)
	if matchErr != nil {
		var me *router.MatchError
		if errors.As(matchErr, &me) {
			if me.Status == http.StatusMethodNotAllowed {
				if len(me.Allowed) > 0 {
					w.Header().Set("Allow", strings.Join(me.Allowed, ", "))
				}
				s.writeError(w, r, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "Method not allowed.", 0)
				return
			}
			s.writeError(w, r, http.StatusNotFound, httpx.CodeRouteNotFound, "Route not found.", 0)
			return
		}
		s.internalError(w, r, fmt.Errorf("route match: %w", matchErr))
		return
	}
	route := matched.Config
	info := httpx.InfoFrom(r.Context())
	if info != nil {
		info.Route = route.Name
	}

	principal, ok := s.authenticate(w, r, route, info)
	if !ok {
		return
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
	if info != nil {
		info.Principal = principal.LogID()
	}
	applyIdentityHeaders(r, principal)
	if err := auth.Authorize(principal, route.Auth.Required, route.Auth.Scopes, route.Auth.Roles, route.Auth.Match); err != nil {
		s.authorizationFailure(w, r, route, info, err)
		return
	}

	if !s.allowRate(w, r, route, info, principal) {
		return
	}
	s.proxy.Forward(w, r, route)
}

func (s *state) acquire(w http.ResponseWriter, r *http.Request) bool {
	if s.sem == nil {
		return true
	}
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		s.writeError(w, r, http.StatusServiceUnavailable, httpx.CodeOverloaded,
			"Gateway overloaded; retry shortly.", time.Second)
		return false
	}
}

func (s *state) release() {
	if s.sem != nil {
		<-s.sem
	}
}

func (s *state) authenticate(w http.ResponseWriter, r *http.Request, route *config.Route, info *httpx.Info) (*auth.Principal, bool) {
	p, err := s.auth.Authenticate(r, route.Auth)
	if err == nil {
		return p, true
	}
	reason := "internal"
	status := http.StatusUnauthorized
	code := httpx.CodeUnauthorized
	message := "Invalid or expired credentials."
	var ae *auth.Error
	if errors.As(err, &ae) {
		reason = string(ae.Reason)
		switch ae.Reason {
		case auth.ReasonForbidden:
			status = http.StatusForbidden
			code = httpx.CodeForbidden
			message = "Forbidden."
		case auth.ReasonMissing:
			message = "Authentication required."
		}
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gateway"`)
	}
	s.metrics.AuthFailure(route.Name, reason)
	if info != nil {
		info.ErrorCode = code
	}
	s.writeError(w, r, status, code, message, 0)
	return nil, false
}

func (s *state) authorizationFailure(w http.ResponseWriter, r *http.Request, route *config.Route, info *httpx.Info, err error) {
	reason := "forbidden"
	var ae *auth.Error
	if errors.As(err, &ae) {
		reason = string(ae.Reason)
	}
	if info != nil {
		info.ErrorCode = httpx.CodeForbidden
	}
	s.metrics.AuthFailure(route.Name, reason)
	s.writeError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Forbidden.", 0)
}

func (s *state) allowRate(w http.ResponseWriter, r *http.Request, route *config.Route, info *httpx.Info, principal *auth.Principal) bool {
	ruleCfg := route.RateLimit
	if ruleCfg == nil {
		return true
	}
	if s.limiter == nil {
		s.logger.Error("route has a rate limit but no limiter is configured", slog.String("route", route.Name))
		s.writeError(w, r, http.StatusServiceUnavailable, httpx.CodeLimiterUnavailable,
			"Rate limiter unavailable.", time.Second)
		return false
	}
	rule := ratelimit.Rule{
		Capacity:     ruleCfg.Capacity,
		RefillPerSec: ruleCfg.RefillPerSec,
		FailOpen:     ruleCfg.FailOpenOr(s.cfg.Redis.FailOpenEnabled()),
	}
	key := s.rateKey(route, ruleCfg, r, info, principal)
	decision, err := s.limiter.Allow(r.Context(), key, rule)
	if err != nil {
		s.metrics.RateLimit(route.Name, metrics.DecisionError)
		s.logger.Warn("rate limiter unavailable",
			slog.String("route", route.Name),
			slog.String("error", err.Error()))
		if r.Context().Err() != nil {
			return false
		}
		s.writeError(w, r, http.StatusServiceUnavailable, httpx.CodeLimiterUnavailable,
			"Rate limiter unavailable.", time.Second)
		return false
	}

	switch {
	case decision.Degraded:
		s.metrics.RateLimit(route.Name, metrics.DecisionFailOpen)
	case decision.Allowed:
		s.metrics.RateLimit(route.Name, metrics.DecisionAllowed)
	default:
		s.metrics.RateLimit(route.Name, metrics.DecisionLimited)
	}

	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.FormatInt(decision.Limit, 10))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
	if decision.ResetAfter > 0 {
		h.Set("X-RateLimit-Reset", strconv.FormatInt(int64(math.Ceil(decision.ResetAfter.Seconds())), 10))
	}
	if decision.Allowed {
		return true
	}

	retry := decision.RetryAfter
	if retry <= 0 {
		retry = time.Second
	}
	secs := int64(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	message := fmt.Sprintf("Too many requests. Retry after %d seconds.", secs)
	s.writeError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited, message, retry)
	return false
}

func (s *state) rateKey(route *config.Route, rule *config.RateLimit, r *http.Request, info *httpx.Info, principal *auth.Principal) string {
	identity := ""
	switch rule.Key {
	case "user", "api_key":
		identity = principal.RateLimitIdentity(rule.Key)
	case "header":
		identity = strings.TrimSpace(r.Header.Get(rule.Header))
	default:
		identity = s.clientIP(r, info)
	}
	if identity == "" {
		identity = s.clientIP(r, info)
	}
	return ratelimit.BuildKey(route.Name, rule.Key, rule.Header, identity)
}

func (s *state) clientIP(r *http.Request, info *httpx.Info) string {
	if info != nil && info.ClientIP != "" {
		return info.ClientIP
	}
	return s.trusted.ClientIP(r)
}

func (s *state) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request handling failed",
		slog.String("error", err.Error()),
		slog.String("request_id", httpx.RequestID(r.Context())))
	s.writeError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "Internal server error.", 0)
}

func (s *state) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, retryAfter time.Duration) {
	httpx.WriteError(w, r, &httpx.Error{Status: status, Code: code, Message: message, RetryAfter: retryAfter})
}

// applyIdentityHeaders removes client-controlled identity headers and writes
// the authenticated identity for trustworthy upstreams.
func applyIdentityHeaders(r *http.Request, p *auth.Principal) {
	httpx.StripIdentityHeaders(r.Header)
	if p == nil || p.IsAnonymous() {
		return
	}
	h := r.Header
	h.Set("X-User-Id", headerSafe(p.ID, maxIdentityLen))
	h.Set("X-User-Kind", headerSafe(p.Kind, maxIdentityLen))
	if len(p.Scopes) > 0 {
		h.Set("X-User-Scopes", headerSafe(strings.Join(p.Scopes, " "), maxScopeLen))
	}
	if len(p.Roles) > 0 {
		h.Set("X-User-Roles", headerSafe(strings.Join(p.Roles, " "), maxScopeLen))
	}
}

func headerSafe(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > max {
		value = value[:max]
	}
	return value
}
