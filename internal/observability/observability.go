// Package observability builds the process logger. Logging policy: structured
// fields only, request IDs on every request-scoped line, and never credentials,
// query strings, cookies or bodies.
package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// NewLogger builds a slog logger from validated config. A nil writer uses
// stdout. version and service are attached to every line.
func NewLogger(cfg config.Log, version string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	if version == "" {
		version = "dev"
	}
	opts := &slog.HandlerOptions{
		Level: ParseLevel(cfg.Level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.Time(slog.TimeKey, t.UTC())
				}
			case slog.LevelKey:
				if l, ok := a.Value.Any().(slog.Level); ok {
					return slog.String(slog.LevelKey, strings.ToLower(l.String()))
				}
			}
			return a
		},
	}

	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With(
		slog.String("service", "gateway"),
		slog.String("version", version),
	)
}

// ParseLevel maps config levels to slog levels. Unknown values fall back to
// info; config validation normally rejects them first.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard returns a logger that drops everything, for tests and tools.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// WithRequestID adds the request ID to a logger when non-empty.
func WithRequestID(logger *slog.Logger, id string) *slog.Logger {
	if logger == nil || id == "" {
		return logger
	}
	return logger.With(slog.String("request_id", id))
}
