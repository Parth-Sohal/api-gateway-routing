// Package proxy forwards matched requests to a selected upstream target. It
// owns per-upstream transports, balancers, breakers, passive health and retry
// budgets, and it maps upstream failures to the gateway error envelope.
//
// Timeouts bound the retry loop and upstream response headers. Once a final
// response starts, its body streams under client cancellation so long-lived
// responses (SSE) are not cut off by the request deadline.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/balancer"
	"github.com/Parth-Sohal/api-gateway-routing/internal/breaker"
	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/health"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/observability"
)

const streamBufferSize = 32 << 10

// Deps are the explicit dependencies of the forwarding layer.
type Deps struct {
	Logger  *slog.Logger
	Metrics *metrics.Metrics
	Clock   clock.Clock
	// Sleep is injectable so retry/backoff tests do not wait. Nil uses real
	// timers honoring context cancellation.
	Sleep func(context.Context, time.Duration) error
}

// Proxy is an immutable set of upstream runtimes.
type Proxy struct {
	upstreams map[string]*Upstream
	logger    *slog.Logger
	metrics   *metrics.Metrics
	clock     clock.Clock
	sleep     func(context.Context, time.Duration) error
	buffers   sync.Pool
}

// Upstream is the runtime for one configured upstream.
type Upstream struct {
	Name      string
	Pool      *balancer.Pool
	Registry  *health.Registry
	Checker   *health.Checker
	Breaker   *breaker.Breaker
	Client    *http.Client
	transport *http.Transport

	retry    retryPolicy
	budget   *retryBudget
	count500 bool
}

// Build compiles upstream runtimes from a validated config snapshot.
func Build(cfg *config.Config, deps Deps) (*Proxy, error) {
	if cfg == nil {
		return nil, errors.New("proxy: nil config")
	}
	logger := deps.Logger
	if logger == nil {
		logger = observability.Discard()
	}
	c := deps.Clock
	if c == nil {
		c = clock.Real{}
	}
	sleep := deps.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	p := &Proxy{
		upstreams: make(map[string]*Upstream, len(cfg.Upstreams)),
		logger:    logger,
		metrics:   deps.Metrics,
		clock:     c,
		sleep:     sleep,
	}
	p.buffers.New = func() any { return make([]byte, streamBufferSize) }

	for i := range cfg.Upstreams {
		up, err := p.buildUpstream(&cfg.Upstreams[i])
		if err != nil {
			return nil, err
		}
		if _, dup := p.upstreams[up.Name]; dup {
			return nil, fmt.Errorf("proxy: duplicate upstream %q", up.Name)
		}
		p.upstreams[up.Name] = up
	}
	return p, nil
}

func (p *Proxy) buildUpstream(cfg *config.Upstream) (*Upstream, error) {
	name := cfg.Name
	if name == "" {
		return nil, errors.New("proxy: upstream with empty name")
	}

	registry := health.NewRegistry(
		cfg.HealthCheck.HealthyThreshold,
		cfg.HealthCheck.UnhealthyThreshold,
		p.clock,
		func(ch health.Change) {
			p.metrics.TargetHealthy(name, ch.Target, ch.Available)
			attrs := []any{
				slog.String("upstream", name),
				slog.String("target", ch.Target),
				slog.String("reason", ch.Reason),
			}
			if ch.Available {
				p.logger.Info("target healthy", attrs...)
			} else {
				p.logger.Warn("target unhealthy", attrs...)
			}
		},
	)

	pool, err := balancer.New(name, cfg.Balancer, cfg.Targets, registry)
	if err != nil {
		return nil, err
	}
	for _, t := range pool.Targets() {
		registry.Register(t.ID)
		p.metrics.TargetHealthy(name, t.ID, true)
	}

	var br *breaker.Breaker
	if cfg.Breaker.IsEnabled() {
		bc := cfg.Breaker
		br = breaker.New(breaker.Config{
			FailureThreshold:  bc.FailureThreshold,
			FailureRatio:      bc.FailureRatio,
			MinRequests:       bc.MinRequests,
			Window:            bc.Window.Duration,
			Buckets:           bc.Buckets,
			OpenTimeout:       bc.OpenTimeout.Duration,
			HalfOpenMaxProbes: bc.HalfOpenMaxProbes,
			HalfOpenSuccesses: bc.HalfOpenSuccesses,
		}, p.clock, breaker.Hooks{
			OnStateChange: func(from, to breaker.State, reason string) {
				p.metrics.BreakerState(name, float64(to))
				p.metrics.BreakerTransition(name, from.String(), to.String(), reason)
				attrs := []any{
					slog.String("upstream", name),
					slog.String("from", from.String()),
					slog.String("to", to.String()),
					slog.String("reason", reason),
				}
				switch to {
				case breaker.Open:
					p.logger.Warn("circuit breaker opened", attrs...)
				case breaker.Closed:
					p.logger.Info("circuit breaker closed", attrs...)
				default:
					p.logger.Info("circuit breaker half-open", attrs...)
				}
			},
		})
		p.metrics.BreakerState(name, float64(breaker.Closed))
	}

	tr := newTransport(cfg.Transport)
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var checker *health.Checker
	if cfg.HealthCheck.Enabled {
		targets := make([]health.Target, 0, len(pool.Targets()))
		for _, t := range pool.Targets() {
			targets = append(targets, health.Target{ID: t.ID, URL: t.URL})
		}
		checker, err = health.NewChecker(cfg.HealthCheck, targets, registry, nil, p.logger, func(res health.ProbeResult) {
			result := metrics.ProbeSuccess
			if !res.Healthy {
				result = metrics.ProbeFailure
			}
			p.metrics.Probe(name, res.Target, result, res.Duration)
		})
		if err != nil {
			tr.CloseIdleConnections()
			return nil, err
		}
	}

	return &Upstream{
		Name:      name,
		Pool:      pool,
		Registry:  registry,
		Checker:   checker,
		Breaker:   br,
		Client:    client,
		transport: tr,
		retry:     newRetryPolicy(cfg.Retry),
		budget:    newRetryBudget(cfg.Retry.BudgetPercent),
		count500:  cfg.Breaker.Count500,
	}, nil
}

func newTransport(c config.Transport) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   c.DialTimeout.Duration,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          c.MaxIdleConns,
		MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
		MaxConnsPerHost:       c.MaxConnsPerHost,
		IdleConnTimeout:       c.IdleConnTimeout.Duration,
		TLSHandshakeTimeout:   c.TLSHandshakeTimeout.Duration,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout.Duration,
		ExpectContinueTimeout: c.ExpectContinueTimeout.Duration,
		DisableCompression:    true,
	}
}

// Upstream returns a configured upstream by name.
func (p *Proxy) Upstream(name string) (*Upstream, bool) {
	up, ok := p.upstreams[name]
	return up, ok
}

// Names returns configured upstream names in sorted order.
func (p *Proxy) Names() []string {
	names := make([]string, 0, len(p.upstreams))
	for name := range p.upstreams {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// UpstreamSnapshot is an admin/metrics view of one upstream.
type UpstreamSnapshot struct {
	Name    string
	Targets []balancer.TargetSnapshot
	Health  []health.Status
	Breaker *breaker.Snapshot
}

// Snapshots returns a sorted, consistent view for the admin API.
func (p *Proxy) Snapshots() []UpstreamSnapshot {
	out := make([]UpstreamSnapshot, 0, len(p.upstreams))
	for _, name := range p.Names() {
		up := p.upstreams[name]
		snap := UpstreamSnapshot{
			Name:    name,
			Targets: up.Pool.Snapshot(),
			Health:  up.Registry.Snapshot(),
		}
		if up.Breaker != nil {
			b := up.Breaker.Snapshot()
			snap.Breaker = &b
		}
		out = append(out, snap)
	}
	return out
}

// RunHealthChecks runs every active checker until ctx is cancelled. It is
// intended to be started by the server state that owns this proxy.
func (p *Proxy) RunHealthChecks(ctx context.Context) {
	var wg sync.WaitGroup
	for _, up := range p.upstreams {
		if up.Checker == nil {
			continue
		}
		checker := up.Checker
		wg.Add(1)
		go func() {
			defer wg.Done()
			checker.Run(ctx)
		}()
	}
	wg.Wait()
}

// Close releases idle connections. Call after the health-check context is
// cancelled.
func (p *Proxy) Close() {
	for _, up := range p.upstreams {
		up.transport.CloseIdleConnections()
		if up.Checker != nil {
			up.Checker.CloseIdleConnections()
		}
	}
}

type attemptResult struct {
	resp     *http.Response
	err      error
	target   *balancer.Target
	status   int
	reason   string
	timedOut bool
	canceled bool
}

// Forward proxies one request according to route. It writes either an upstream
// response or a gateway error envelope.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, route *config.Route) {
	if route == nil {
		p.writeError(w, r, "", "", http.StatusInternalServerError, httpx.CodeInternalError,
			"Internal server error.", 0, errors.New("nil route"))
		return
	}
	up, ok := p.upstreams[route.Upstream]
	if !ok {
		p.logger.Error("route references unknown upstream",
			slog.String("route", route.Name), slog.String("upstream", route.Upstream))
		p.writeError(w, r, route.Upstream, "", http.StatusInternalServerError, httpx.CodeInternalError,
			"Internal server error.", 0, fmt.Errorf("unknown upstream %q", route.Upstream))
		return
	}

	maxBody := route.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	willRetry := up.retry.allowsMethod(r.Method)
	body, err := prepareRequestBody(w, r, up.retry, willRetry, maxBody)
	if err != nil {
		status, code, msg := http.StatusBadRequest, httpx.CodeInvalidRequest, "Invalid request body."
		if errors.Is(err, errBodyTooLarge) {
			status, code, msg = http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "Request body too large."
		}
		p.writeError(w, r, up.Name, "", status, code, msg, 0, err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timer *time.Timer
	if route.Timeout.Duration > 0 {
		timer = time.AfterFunc(route.Timeout.Duration, cancel)
	}
	if timer != nil {
		defer timer.Stop()
	}

	up.budget.deposit()
	maxAttempts := 1
	if willRetry {
		maxAttempts = up.retry.maxAttempts
		if !body.replayable {
			maxAttempts = 1
		}
	}
	attempted := make(map[string]struct{}, maxAttempts)
	retries := 0

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			p.deadlineFailure(w, r, up.Name, "")
			return
		}

		res := p.attempt(ctx, r, route, up, body, attempted)
		p.setInfo(r, up.Name, res.target, retries)

		if res.err != nil {
			retryable := (res.reason == "timeout" || res.reason == "connection_error") &&
				up.retry.retryError(res.timedOut, res.canceled) &&
				attempt < maxAttempts && ctx.Err() == nil && body.replayable
			if retryable && up.budget.allow() {
				retries++
				p.metrics.Retry(up.Name, res.reason)
				p.setInfo(r, up.Name, res.target, retries)
				if err := p.sleep(ctx, up.retry.backoff(attempt)); err != nil {
					p.deadlineFailure(w, r, up.Name, targetID(res.target))
					return
				}
				continue
			}
			p.failAttempt(w, r, up, res)
			p.setInfo(r, up.Name, res.target, retries)
			return
		}

		if attempt < maxAttempts && up.retry.retryStatus(res.status) && body.replayable && ctx.Err() == nil {
			if up.budget.allow() {
				retries++
				p.metrics.Retry(up.Name, strconv.Itoa(res.status))
				drainAndClose(res.resp.Body, 4<<10)
				p.setInfo(r, up.Name, res.target, retries)
				if err := p.sleep(ctx, up.retry.backoff(attempt)); err != nil {
					p.deadlineFailure(w, r, up.Name, targetID(res.target))
					return
				}
				continue
			}
		}

		// A response is final: stop the header/retry timer so a long body can
		// stream under client cancellation.
		if timer != nil && !timer.Stop() {
			_ = res.resp.Body.Close()
			p.deadlineFailure(w, r, up.Name, targetID(res.target))
			return
		}
		p.setInfo(r, up.Name, res.target, retries)
		p.writeResponse(w, r, up, res)
		return
	}
}

func (p *Proxy) attempt(ctx context.Context, r *http.Request, route *config.Route, up *Upstream, body *requestBody, attempted map[string]struct{}) (res attemptResult) {
	if up.Breaker != nil {
		done, err := up.Breaker.Allow()
		if err != nil {
			res.err = err
			res.reason = "breaker_open"
			return res
		}
		defer func() { done(breakerOutcome(res, up.count500)) }()
	}

	target, err := up.Pool.Acquire(attempted)
	if err != nil && len(attempted) > 0 {
		// Every target was already tried: allow a second pass rather than
		// failing a single-target upstream outright.
		target, err = up.Pool.Acquire(nil)
	}
	if err != nil {
		res.err = err
		res.reason = "no_targets"
		return res
	}
	res.target = target
	attempted[target.ID] = struct{}{}

	reader, length, ok := body.open()
	if !ok {
		up.Pool.Release(target)
		res.err = errors.New("proxy: request body is not replayable")
		res.reason = "body"
		return res
	}
	req, err := buildUpstreamRequest(ctx, r, route, target, reader, length, body.getBody())
	if err != nil {
		up.Pool.Release(target)
		res.err = err
		res.reason = "request"
		return res
	}

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		up.Pool.Release(target)
		p.metrics.UpstreamInflight(up.Name, target.ID, -1)
	}
	p.metrics.UpstreamInflight(up.Name, target.ID, 1)

	start := time.Now()
	resp, err := up.Client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		release()
		res.canceled = r.Context().Err() != nil
		res.timedOut = !res.canceled && (isTimeout(err) || ctx.Err() != nil)
		switch {
		case res.canceled:
			res.reason = "canceled"
		case res.timedOut:
			res.reason = "timeout"
		default:
			res.reason = "connection_error"
		}
		res.err = err
		if !res.canceled && up.Registry != nil {
			up.Registry.Failure(target.ID, shortError(err))
		}
		outcome := metrics.OutcomeTransportError
		switch res.reason {
		case "timeout":
			outcome = metrics.OutcomeTimeout
		case "canceled":
			outcome = metrics.OutcomeCanceled
		}
		p.metrics.UpstreamAttempt(up.Name, target.ID, outcome, elapsed)
		return res
	}

	resp.Body = &releaseBody{ReadCloser: resp.Body, release: release}
	res.resp = resp
	res.status = resp.StatusCode
	p.metrics.UpstreamAttempt(up.Name, target.ID, statusOutcome(resp.StatusCode), elapsed)
	if up.Registry != nil {
		if resp.StatusCode >= 500 {
			up.Registry.Failure(target.ID, "status "+strconv.Itoa(resp.StatusCode))
		} else {
			up.Registry.Success(target.ID)
		}
	}
	return res
}

func breakerOutcome(res attemptResult, count500 bool) error {
	if res.err != nil {
		if res.canceled {
			return breaker.ErrIgnored
		}
		return res.err
	}
	if res.resp == nil {
		return errors.New("proxy: empty attempt result")
	}
	switch {
	case res.status == http.StatusInternalServerError && !count500:
		return breaker.ErrIgnored
	case res.status >= 500:
		return fmt.Errorf("upstream status %d", res.status)
	case res.status >= 400:
		return breaker.ErrIgnored
	default:
		return nil
	}
}

func statusOutcome(status int) string {
	switch {
	case status >= 500:
		return metrics.OutcomeServerError
	case status >= 400:
		return metrics.OutcomeClientError
	default:
		return metrics.OutcomeSuccess
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func targetID(t *balancer.Target) string {
	if t == nil {
		return ""
	}
	return t.ID
}

func (p *Proxy) setInfo(r *http.Request, upstream string, target *balancer.Target, retries int) {
	info := httpx.InfoFrom(r.Context())
	if info == nil {
		return
	}
	info.Upstream = upstream
	if target != nil {
		info.Target = target.ID
	}
	info.Retries = retries
}

func (p *Proxy) deadlineFailure(w http.ResponseWriter, r *http.Request, upstream, target string) {
	if r.Context().Err() != nil {
		return
	}
	p.writeError(w, r, upstream, target, http.StatusGatewayTimeout,
		httpx.CodeGatewayTimeout, "Upstream timed out.", 0, context.DeadlineExceeded)
}

func (p *Proxy) failAttempt(w http.ResponseWriter, r *http.Request, up *Upstream, res attemptResult) {
	target := targetID(res.target)
	if res.canceled || r.Context().Err() != nil {
		return
	}

	var openErr *breaker.OpenError
	var maxBytes *http.MaxBytesError
	switch {
	case errors.Is(res.err, balancer.ErrNoTargets):
		p.writeError(w, r, up.Name, target, http.StatusServiceUnavailable,
			httpx.CodeNoHealthyTargets, "No healthy upstream targets.", time.Second, res.err)
	case errors.As(res.err, &openErr):
		retryAfter := openErr.RetryAfter
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		p.writeError(w, r, up.Name, target, http.StatusServiceUnavailable,
			httpx.CodeUpstreamUnavailable, "Upstream unavailable.", retryAfter, res.err)
	case errors.As(res.err, &maxBytes), errors.Is(res.err, errBodyTooLarge):
		p.writeError(w, r, up.Name, target, http.StatusRequestEntityTooLarge,
			httpx.CodePayloadTooLarge, "Request body too large.", 0, res.err)
	case errors.Is(res.err, errReadBody):
		p.writeError(w, r, up.Name, target, http.StatusBadRequest,
			httpx.CodeInvalidRequest, "Invalid request body.", 0, res.err)
	case res.timedOut:
		p.writeError(w, r, up.Name, target, http.StatusGatewayTimeout,
			httpx.CodeGatewayTimeout, "Upstream timed out.", 0, res.err)
	default:
		p.writeError(w, r, up.Name, target, http.StatusBadGateway,
			httpx.CodeBadGateway, "Bad gateway.", 0, res.err)
	}
}

func (p *Proxy) writeError(w http.ResponseWriter, r *http.Request, upstream, target string, status int, code, message string, retryAfter time.Duration, cause error) {
	if info := httpx.InfoFrom(r.Context()); info != nil {
		if upstream != "" {
			info.Upstream = upstream
		}
		if target != "" {
			info.Target = target
		}
	}
	httpx.WriteError(w, r, &httpx.Error{
		Status:     status,
		Code:       code,
		Message:    message,
		RetryAfter: retryAfter,
		Err:        cause,
	})
}

func (p *Proxy) writeResponse(w http.ResponseWriter, r *http.Request, up *Upstream, res attemptResult) {
	defer res.resp.Body.Close()

	info := httpx.InfoFrom(r.Context())
	copyResponseHeaders(w.Header(), res.resp.Header, info)
	w.WriteHeader(res.status)

	contentType := strings.ToLower(res.resp.Header.Get("Content-Type"))
	streaming := strings.HasPrefix(contentType, "text/event-stream")
	controller := http.NewResponseController(w)
	if streaming {
		_ = controller.Flush()
	}

	buf := p.buffers.Get().([]byte)
	defer p.buffers.Put(buf)

	var err error
	if streaming {
		err = p.copyStreaming(w, controller, res.resp.Body, buf)
	} else {
		_, err = io.CopyBuffer(w, res.resp.Body, buf)
	}
	if err != nil && r.Context().Err() == nil && !errors.Is(err, io.EOF) {
		p.logger.Debug("response stream interrupted",
			slog.String("upstream", up.Name),
			slog.String("request_id", httpx.RequestID(r.Context())),
			slog.String("error", shortError(err)))
	}
}

func (p *Proxy) copyStreaming(w http.ResponseWriter, rc *http.ResponseController, body io.Reader, buf []byte) error {
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return ferr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

type releaseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func drainAndClose(body io.ReadCloser, limit int64) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit))
	_ = body.Close()
}

func shortError(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
