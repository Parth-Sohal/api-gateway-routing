// Package server owns process lifecycle: it builds immutable serving states
// from validated config, serves public/admin/metrics listeners, atomically
// swaps state on reload, and drains connections before exit.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Parth-Sohal/api-gateway-routing/internal/admin"
	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/middleware"
	"github.com/Parth-Sohal/api-gateway-routing/internal/ratelimit"
)

const (
	redisProbeInterval = 5 * time.Second
	redisProbeTimeout  = 2 * time.Second
	adminIdleTimeout   = 60 * time.Second
	metricsIdleTimeout = 60 * time.Second
	metricsGrace       = 5 * time.Second
)

// Options configure a Server. ConfigPath is optional when an already parsed
// Config is supplied; without a path, reload is unavailable.
type Options struct {
	ConfigPath   string
	Config       *config.Config
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
	Clock        clock.Clock
	Version      string
	DisableWatch bool
}

// AddrSet reports the bound listener addresses. Disabled listeners are empty.
type AddrSet struct {
	Public  string
	Admin   string
	Metrics string
}

// Server is the process-level gateway runtime.
type Server struct {
	configPath   string
	logger       *slog.Logger
	metrics      *metrics.Metrics
	clock        clock.Clock
	version      string
	disableWatch bool
	startedAt    time.Time

	initial *config.Config

	baseCtx    context.Context
	baseCancel context.CancelFunc
	local      *ratelimit.LocalLimiter

	redisMu sync.Mutex
	redis   *redis.Client

	state atomic.Pointer[state]

	reloadMu sync.Mutex
	metaMu   sync.Mutex
	reloads  uint64
	lastLoad time.Time
	lastErr  string

	startMu sync.Mutex
	started bool

	lifecycleMu sync.Mutex

	draining atomic.Bool
	redisOK  atomic.Bool

	connMu     sync.Mutex
	connStates map[net.Conn]http.ConnState

	adminHandler *admin.Handler
	addrs        AddrSet
	listeners    []net.Listener
	httpServers  []*http.Server
	graces       []time.Duration
	serveWG      sync.WaitGroup
	bgWG         sync.WaitGroup

	errMu  sync.Mutex
	runErr error

	shutdownOnce sync.Once
	shutdownMu   sync.Mutex
	shutdownErr  error
	done         chan struct{}
}

// New validates options and parses the initial config when needed. Listener
// configuration (addresses and timeouts) is captured at startup; reload swaps
// routing, auth, limits and upstream policy, not listener sockets.
func New(opts Options) (*Server, error) {
	if opts.Config == nil && strings.TrimSpace(opts.ConfigPath) == "" {
		return nil, errors.New("server: config or config path is required")
	}
	cfg := opts.Config
	if cfg == nil {
		loaded, err := config.LoadFile(opts.ConfigPath)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c := opts.Clock
	if c == nil {
		c = clock.Real{}
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	m := opts.Metrics
	if m == nil {
		m = metrics.New(version)
	}
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &Server{
		configPath:   opts.ConfigPath,
		logger:       logger,
		metrics:      m,
		clock:        c,
		version:      version,
		disableWatch: opts.DisableWatch,
		startedAt:    c.Now(),
		initial:      cfg,
		baseCtx:      baseCtx,
		baseCancel:   baseCancel,
		local:        ratelimit.NewLocal(c),
		connStates:   make(map[net.Conn]http.ConnState),
		done:         make(chan struct{}),
	}, nil
}

// Start builds the first state and binds every enabled listener. It returns
// once the listeners are accepting connections and health checks are running.
func (s *Server) Start(ctx context.Context) (retErr error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.startMu.Lock()
	defer s.startMu.Unlock()
	defer func() {
		if retErr != nil {
			// A failed Start may have already cancelled the server's base
			// context; make the failure terminal instead of leaving a
			// half-initialized runtime that retries would reuse.
			s.draining.Store(true)
		}
	}()
	if s.draining.Load() {
		return errors.New("server: cannot start after shutdown")
	}
	if s.started {
		return errors.New("server: already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	limiter, mode, err := s.limiterFor(s.initial)
	if err != nil {
		s.baseCancel()
		return err
	}
	st, err := newState(s.initial, stateDeps{
		Logger: s.logger, Metrics: s.metrics, Limiter: limiter, LimiterMode: mode,
	})
	if err != nil {
		s.baseCancel()
		s.closeRedis()
		return err
	}
	s.state.Store(st)

	cleanup := func() {
		s.state.Store(nil)
		st.Close()
		s.baseCancel()
		s.closeRedis()
	}

	adminHandler, err := s.buildAdmin(s.initial)
	if err != nil {
		cleanup()
		return err
	}
	s.adminHandler = adminHandler

	publicSrv, publicLn, err := s.listenPublic(s.initial)
	if err != nil {
		cleanup()
		return err
	}
	s.listeners = append(s.listeners, publicLn)
	s.httpServers = append(s.httpServers, publicSrv)
	s.graces = append(s.graces, s.initial.Server.ShutdownGrace.Duration)
	s.addrs.Public = publicLn.Addr().String()

	if s.initial.Metrics.IsEnabled() {
		srv, ln, err := s.listenMetrics(s.initial)
		if err != nil {
			s.closeListeners()
			cleanup()
			return err
		}
		s.listeners = append(s.listeners, ln)
		s.httpServers = append(s.httpServers, srv)
		s.graces = append(s.graces, metricsGrace)
		s.addrs.Metrics = ln.Addr().String()
	}

	if adminHandler != nil {
		srv, ln, err := s.listenAdmin(s.initial, adminHandler)
		if err != nil {
			s.closeListeners()
			cleanup()
			return err
		}
		s.listeners = append(s.listeners, ln)
		s.httpServers = append(s.httpServers, srv)
		s.graces = append(s.graces, s.initial.Admin.ShutdownGrace.Duration)
		s.addrs.Admin = ln.Addr().String()
	}

	s.local.Start(s.baseCtx)
	var watcher *config.Watcher
	if s.configPath != "" && !s.disableWatch {
		watcher, err = config.NewWatcher(s.configPath, s.logger, func() error {
			return s.Reload(context.Background())
		})
		if err != nil {
			s.closeListeners()
			cleanup()
			return err
		}
	}

	for i := range s.httpServers {
		srv := s.httpServers[i]
		ln := s.listeners[i]
		s.serveWG.Add(1)
		go func() {
			defer s.serveWG.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("listener failed", slog.String("addr", srv.Addr), slog.String("error", err.Error()))
				s.setRunErr(err)
				go s.Shutdown(context.Background())
			}
		}()
	}

	if watcher != nil {
		s.bgWG.Add(1)
		go func() {
			defer s.bgWG.Done()
			if err := watcher.Run(s.baseCtx); err != nil && s.baseCtx.Err() == nil {
				s.logger.Error("config watcher stopped", slog.String("error", err.Error()))
			}
		}()
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.probeRedis(s.baseCtx)
	}()

	s.redisOK.Store(true)
	s.started = true
	s.logger.Info("gateway started",
		slog.String("version", s.version),
		slog.String("public_addr", s.addrs.Public),
		slog.String("admin_addr", s.addrs.Admin),
		slog.String("metrics_addr", s.addrs.Metrics),
		slog.String("rate_limiter", mode))
	return nil
}

// Run starts the server and blocks until ctx is cancelled or a listener fails.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		_ = s.Shutdown(context.Background())
		return s.Err()
	case <-s.done:
		return s.Err()
	}
}

// Shutdown stops accepting new connections, drains in-flight requests within
// the configured grace periods, stops background work and closes shared
// resources. It is idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		// Wait for Start to finish wiring listeners so a listener failure
		// during startup cannot shut down a half-built server.
		s.lifecycleMu.Lock()
		s.lifecycleMu.Unlock()

		s.draining.Store(true)
		if ctx == nil {
			ctx = context.Background()
		}

		var wg sync.WaitGroup
		for i, srv := range s.httpServers {
			grace := metricsGrace
			if i < len(s.graces) {
				grace = s.graces[i]
			}
			wg.Add(1)
			go func(srv *http.Server, grace time.Duration) {
				defer wg.Done()
				runCtx, cancel := context.WithTimeout(ctx, grace)
				defer cancel()
				if err := srv.Shutdown(runCtx); err != nil {
					_ = srv.Close()
					s.setShutdownErr(err)
					s.logger.Warn("graceful shutdown timed out",
						slog.String("addr", srv.Addr), slog.String("error", err.Error()))
				}
			}(srv, grace)
		}
		wg.Wait()
		s.serveWG.Wait()

		if st := s.state.Swap(nil); st != nil {
			st.Close()
		}
		s.baseCancel()
		s.bgWG.Wait()
		s.closeRedis()

		s.startMu.Lock()
		s.started = false
		s.startMu.Unlock()
		s.logger.Info("gateway stopped")
		close(s.done)
	})
	return s.ShutdownErr()
}

// Reload rebuilds state from the config file and atomically swaps it in. Any
// failure leaves the previous state serving.
func (s *Server) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	start := s.clock.Now()
	if s.draining.Load() {
		err := errors.New("server is shutting down")
		s.recordReload(start, err)
		return err
	}
	if strings.TrimSpace(s.configPath) == "" {
		err := errors.New("reload requires a config file path")
		s.recordReload(start, err)
		return err
	}
	cfg, err := config.LoadFile(s.configPath)
	if err == nil {
		err = s.activate(cfg, ctx)
	}
	s.recordReload(start, err)
	return err
}

func (s *Server) activate(cfg *config.Config, ctx context.Context) error {
	limiter, mode, err := s.limiterFor(cfg)
	if err != nil {
		return err
	}
	st, err := newState(cfg, stateDeps{
		Logger: s.logger, Metrics: s.metrics, Limiter: limiter, LimiterMode: mode,
	})
	if err != nil {
		return err
	}
	if ctx != nil && ctx.Err() != nil {
		st.Close()
		return ctx.Err()
	}
	old := s.state.Swap(st)
	if old != nil {
		old.Close()
	}
	s.redisOK.Store(true)
	s.logger.Info("config reloaded", slog.Int("routes", len(cfg.Routes)), slog.Int("upstreams", len(cfg.Upstreams)))
	return nil
}

func (s *Server) recordReload(start time.Time, err error) {
	dur := s.clock.Now().Sub(start)
	now := s.clock.Now()
	s.metrics.ConfigReload(err == nil, dur, now)
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
		return
	}
	s.reloads++
	s.lastLoad = now
	s.lastErr = ""
}

// AdminSnapshot implements admin.Source.
func (s *Server) AdminSnapshot() admin.Snapshot {
	snap := admin.Snapshot{RateLimiter: "disabled"}
	if st := s.state.Load(); st != nil {
		snap.Config = st.cfg
		snap.Upstreams = st.proxy.Snapshots()
		snap.RateLimiter = st.limiterMode
	}
	s.metaMu.Lock()
	snap.Reloads = s.reloads
	snap.LastReload = s.lastLoad
	snap.LastReloadError = s.lastErr
	s.metaMu.Unlock()
	return snap
}

// Ready implements admin.Source and readiness probes.
func (s *Server) Ready() (bool, string) {
	if s.draining.Load() {
		return false, "shutting down"
	}
	s.startMu.Lock()
	started := s.started
	s.startMu.Unlock()
	if !started {
		return false, "starting"
	}
	st := s.state.Load()
	if st == nil {
		return false, "no active configuration"
	}
	if st.cfg.HasStrictRateLimits() && !s.redisOK.Load() {
		return false, "redis unavailable"
	}
	return true, ""
}

// Err returns the listener error that triggered shutdown, if any.
func (s *Server) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.runErr
}

// ShutdownErr returns an error when graceful drain timed out.
func (s *Server) ShutdownErr() error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	return s.shutdownErr
}

// Addrs returns the bound listener addresses.
func (s *Server) Addrs() AddrSet { return s.addrs }

// Done is closed after shutdown completes.
func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) setRunErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.runErr == nil {
		s.runErr = err
	}
	s.errMu.Unlock()
}

func (s *Server) setShutdownErr(err error) {
	if err == nil {
		return
	}
	s.shutdownMu.Lock()
	if s.shutdownErr == nil {
		s.shutdownErr = err
	}
	s.shutdownMu.Unlock()
}

func (s *Server) limiterFor(cfg *config.Config) (ratelimit.Limiter, string, error) {
	if !cfg.HasRateLimits() {
		return nil, "disabled", nil
	}
	client, err := s.redisFor(cfg.Redis)
	if err != nil {
		return nil, "", err
	}
	if cfg.HasStrictRateLimits() {
		timeout := cfg.Redis.DialTimeout.Duration + cfg.Redis.OpTimeout.Duration
		if timeout < redisProbeTimeout {
			timeout = redisProbeTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := client.Ping(ctx).Err()
		cancel()
		if err != nil {
			s.redisOK.Store(false)
			return nil, "", fmt.Errorf("redis is required by fail-closed rate limits: %w", err)
		}
	}
	return ratelimit.NewFallback(ratelimit.FallbackOptions{
		Primary: ratelimit.NewRedis(client),
		Local:   s.local,
		Timeout: cfg.Redis.OpTimeout.Duration,
	}), "redis", nil
}

func (s *Server) redisFor(cfg config.Redis) (*redis.Client, error) {
	s.redisMu.Lock()
	defer s.redisMu.Unlock()
	if s.redis != nil {
		return s.redis, nil
	}
	password := ""
	if cfg.PasswordEnv != "" {
		password = os.Getenv(cfg.PasswordEnv)
		if password == "" {
			return nil, fmt.Errorf("redis password environment variable %s is empty", cfg.PasswordEnv)
		}
	}
	client := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Password:        password,
		DB:              cfg.DB,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		DialTimeout:     cfg.DialTimeout.Duration,
		ReadTimeout:     cfg.ReadTimeout.Duration,
		WriteTimeout:    cfg.WriteTimeout.Duration,
		MaxRetries:      1,
		DisableIdentity: true,
	})
	s.redis = client
	return client, nil
}

func (s *Server) closeRedis() {
	s.redisMu.Lock()
	client := s.redis
	s.redis = nil
	s.redisMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (s *Server) probeRedis(ctx context.Context) {
	ticker := time.NewTicker(redisProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkRedis(ctx)
		}
	}
}

func (s *Server) checkRedis(ctx context.Context) {
	st := s.state.Load()
	if st == nil || !st.cfg.HasStrictRateLimits() {
		s.redisOK.Store(true)
		return
	}
	s.redisMu.Lock()
	client := s.redis
	s.redisMu.Unlock()
	if client == nil {
		s.redisOK.Store(false)
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, redisProbeTimeout)
	err := client.Ping(probeCtx).Err()
	cancel()
	was := s.redisOK.Swap(err == nil)
	if err != nil && was {
		s.logger.Warn("redis readiness probe failed", slog.String("error", err.Error()))
	}
	if err == nil && !was {
		s.logger.Info("redis readiness restored")
	}
}

func (s *Server) buildAdmin(cfg *config.Config) (*admin.Handler, error) {
	if !cfg.Admin.IsEnabled() {
		return nil, nil
	}
	token := strings.TrimSpace(os.Getenv(cfg.Admin.TokenEnv))
	if token == "" {
		return nil, fmt.Errorf("admin token environment variable %s is empty", cfg.Admin.TokenEnv)
	}
	h, err := admin.New(admin.Deps{
		Logger:    s.logger.With(slog.String("listener", "admin")),
		Metrics:   s.metrics,
		Clock:     s.clock,
		Source:    s,
		Token:     token,
		Version:   s.version,
		StartedAt: s.startedAt,
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (s *Server) listenPublic(cfg *config.Config) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen public %s: %w", cfg.Server.Addr, err)
	}
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           http.HandlerFunc(s.dynamicHandler),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration,
		WriteTimeout:      cfg.Server.WriteTimeout.Duration,
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return s.baseCtx },
		ConnState:         s.trackConn,
	}
	return srv, ln, nil
}

func (s *Server) listenAdmin(cfg *config.Config, h *admin.Handler) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", cfg.Admin.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen admin %s: %w", cfg.Admin.Addr, err)
	}
	trusted, err := httpx.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := httpx.InfoFrom(r.Context()); info != nil {
			info.Route = "admin"
		}
		h.ServeHTTP(w, r)
	})
	handler := middleware.Chain(
		base,
		middleware.RequestID(trusted),
		middleware.AccessLog(middleware.AccessConfig{
			Enabled: cfg.Log.AccessLogEnabled(),
			Logger:  s.logger.With(slog.String("listener", "admin")),
			Metrics: s.metrics,
		}),
		middleware.Recovery(s.logger, s.metrics),
	)
	srv := &http.Server{
		Addr:              cfg.Admin.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Admin.ReadHeaderTimeout.Duration,
		IdleTimeout:       adminIdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return s.baseCtx },
	}
	return srv, ln, nil
}

func (s *Server) listenMetrics(cfg *config.Config) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", cfg.Metrics.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen metrics %s: %w", cfg.Metrics.Addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle(cfg.Metrics.Path, s.metrics.Handler())
	defaultHandler := mux
	srv := &http.Server{
		Addr:              cfg.Metrics.Addr,
		Handler:           defaultHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       metricsIdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return s.baseCtx },
	}
	return srv, ln, nil
}

func (s *Server) dynamicHandler(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if st == nil {
		httpx.WriteError(w, r, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeInternalError, "Gateway is not ready."))
		return
	}
	st.ServeHTTP(w, r)
}

func (s *Server) closeListeners() {
	for _, ln := range s.listeners {
		_ = ln.Close()
	}
	s.listeners = nil
	s.httpServers = nil
	s.graces = nil
	s.addrs = AddrSet{}
}

func (s *Server) trackConn(c net.Conn, st http.ConnState) {
	label := func(state http.ConnState) string {
		switch state {
		case http.StateActive:
			return "active"
		case http.StateIdle:
			return "idle"
		default:
			return ""
		}
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	prev, tracked := s.connStates[c]
	if tracked {
		if prevLabel := label(prev); prevLabel != "" {
			s.metrics.ConnectionState(prevLabel, -1)
		}
	}
	if st == http.StateClosed || st == http.StateHijacked {
		delete(s.connStates, c)
		return
	}
	s.connStates[c] = st
	if next := label(st); next != "" {
		s.metrics.ConnectionState(next, 1)
	}
}
