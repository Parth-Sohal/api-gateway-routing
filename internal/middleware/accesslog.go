package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

// AccessConfig controls the access-log middleware.
type AccessConfig struct {
	Enabled bool
	Logger  *slog.Logger
	Metrics *metrics.Metrics
}

// AccessLog records status, bytes, duration and metrics for every request.
// Paths are logged without query strings, and control characters are stripped
// so a hostile request cannot forge log lines.
func AccessLog(cfg AccessConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httpx.NewRecorder(w)
			start := time.Now()
			cfg.Metrics.HTTPInflight(1)
			defer cfg.Metrics.HTTPInflight(-1)

			defer func() {
				status := rec.Status()
				if status == 0 {
					status = http.StatusOK
				}
				dur := time.Since(start)
				// The route is assigned by the handler after matching, so read
				// the info again here.
				info := httpx.InfoFrom(r.Context())
				route := routeLabel(info)
				cfg.Metrics.ObserveHTTP(route, r.Method, status, dur, rec.Bytes())
				if !cfg.Enabled || cfg.Logger == nil {
					return
				}

				attrs := []any{
					slog.String("method", logSafe(r.Method, 32)),
					slog.String("path", logSafe(r.URL.Path, 1024)),
					slog.String("route", route),
					slog.Int("status", status),
					slog.Int64("bytes", rec.Bytes()),
					slog.Float64("duration_ms", float64(dur.Nanoseconds())/1e6),
					slog.String("client_ip", clientIP(info, r)),
				}
				if info != nil {
					attrs = append(attrs, slog.String("request_id", info.RequestID))
					if info.Upstream != "" {
						attrs = append(attrs, slog.String("upstream", info.Upstream))
					}
					if info.Target != "" {
						attrs = append(attrs, slog.String("target", logSafe(info.Target, 512)))
					}
					if info.Retries > 0 {
						attrs = append(attrs, slog.Int("retries", info.Retries))
					}
					if info.ErrorCode != "" {
						attrs = append(attrs, slog.String("error_code", info.ErrorCode))
					}
				}
				cfg.Logger.Info("request", attrs...)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

func routeLabel(info *httpx.Info) string {
	if info == nil || info.Route == "" {
		return "unmatched"
	}
	return info.Route
}

func clientIP(info *httpx.Info, r *http.Request) string {
	if info != nil && info.ClientIP != "" {
		return info.ClientIP
	}
	return httpx.ClientIP(r, httpx.TrustedProxies{})
}

// logSafe removes ASCII control characters and caps the value length.
func logSafe(s string, max int) string {
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		for i := range s {
			if i >= max {
				return s[:i]
			}
		}
	}
	return s
}
