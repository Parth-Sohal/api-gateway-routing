// Command mockupstream is a small demo/benchmark backend. It echoes request
// metadata and exposes deterministic fault endpoints used by the chaos and
// load-test scenarios. It never echoes credentials or request bodies.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

const (
	maxEchoBody    = 1 << 20
	maxSlowDelay   = time.Minute
	shutdownGrace  = 10 * time.Second
	defaultMockTag = "mock"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mockupstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":9001", "listen address")
	name := fs.String("name", defaultMockTag, "upstream name reported in responses and logs")
	level := fs.String("log-level", "info", "log level (debug, info, warn, error)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mockupstream: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	logger := mockLogger(stdout, *level)
	tag := safeName(*name)

	mock := &mock{name: tag, rand: rand.Float64}
	handler := requestID(accessLog(logger, mock.routes()))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("mock upstream listening", slog.String("addr", *addr), slog.String("name", tag))

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown timed out", slog.String("error", err.Error()))
			_ = srv.Close()
			return 1
		}
		logger.Info("mock upstream stopped")
		return 0
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintf(stderr, "mockupstream: listen %s: %v\n", *addr, err)
		return 1
	}
}

type mock struct {
	name string
	rand func() float64
}

func (m *mock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", m.health)
	mux.HandleFunc("/debug/status", m.status)
	mux.HandleFunc("/debug/slow", m.slow)
	mux.HandleFunc("/debug/flaky", m.flaky)
	mux.HandleFunc("/", m.echo)
	return mux
}

func (m *mock) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "upstream": m.name})
}

func (m *mock) echo(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Drain a bounded amount so keep-alive works without buffering arbitrary
	// uploads in a demo process.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxEchoBody))
	writeJSON(w, http.StatusOK, m.echoBody(r))
}

func (m *mock) status(w http.ResponseWriter, r *http.Request) {
	code := http.StatusOK
	if raw := r.URL.Query().Get("code"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 200 || n > 599 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"upstream": m.name,
				"error":    "code must be an integer between 200 and 599",
			})
			return
		}
		code = n
	}
	if code == http.StatusNoContent || code == http.StatusNotModified {
		w.WriteHeader(code)
		return
	}
	writeJSON(w, code, map[string]any{"upstream": m.name, "status": code})
}

func (m *mock) slow(w http.ResponseWriter, r *http.Request) {
	delay := time.Second
	if raw := r.URL.Query().Get("ms"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"upstream": m.name,
				"error":    "ms must be a non-negative integer",
			})
			return
		}
		delay = time.Duration(ms) * time.Millisecond
	}
	if delay > maxSlowDelay {
		delay = maxSlowDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return
	case <-timer.C:
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream":   m.name,
		"delayed_ms": delay.Milliseconds(),
	})
}

func (m *mock) flaky(w http.ResponseWriter, r *http.Request) {
	rate := 0.3
	if raw := r.URL.Query().Get("rate"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"upstream": m.name,
				"error":    "rate must be a number between 0 and 1",
			})
			return
		}
		rate = v
	}
	if m.rand() < rate {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"upstream": m.name,
			"error":    "injected flaky failure",
		})
		return
	}
	writeJSON(w, http.StatusOK, m.echoBody(r))
}

func (m *mock) echoBody(r *http.Request) map[string]any {
	identity := make(map[string]string, 4)
	for _, header := range []string{"X-User-Id", "X-User-Kind", "X-User-Scopes", "X-User-Roles"} {
		if v := r.Header.Get(header); v != "" {
			identity[header] = v
		}
	}
	body := map[string]any{
		"upstream":   m.name,
		"method":     r.Method,
		"path":       r.URL.Path,
		"host":       r.Host,
		"client_ip":  httpx.ClientIP(r, httpx.TrustedProxies{}),
		"request_id": r.Header.Get(httpx.HeaderRequestID),
		"time":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	if r.URL.RawQuery != "" {
		body["query"] = r.URL.RawQuery
	}
	if len(identity) > 0 {
		body["identity"] = identity
	}
	return body
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.SanitizeRequestID(r.Header.Get(httpx.HeaderRequestID))
		if !ok {
			id = httpx.NewRequestID()
		}
		r.Header.Set(httpx.HeaderRequestID, id)
		w.Header().Set(httpx.HeaderRequestID, id)
		next.ServeHTTP(w, r)
	})
}

func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httpx.NewRecorder(w)
		start := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.Status()
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.Bytes()),
			slog.Float64("duration_ms", float64(time.Since(start).Nanoseconds())/1e6),
			slog.String("request_id", r.Header.Get(httpx.HeaderRequestID)))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return defaultMockTag
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

func mockLogger(w io.Writer, level string) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	var l slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l}))
}
