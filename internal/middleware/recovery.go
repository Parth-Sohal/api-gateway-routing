package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
)

const (
	maxPanicLength = 256
	maxStackLength = 8192
)

// Recovery converts panics into the standard 500 envelope. It re-panics
// http.ErrAbortHandler so the server can abort a connection intentionally.
// Once a response has started the envelope cannot be written; the error is
// logged and the client sees the truncated response.
func Recovery(logger *slog.Logger, m *metrics.Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Without an outer recorder (for example a test or a future
			// listener that disables access logs) we still need to know
			// whether headers have been sent before writing an envelope.
			rw := w
			if _, ok := rw.(interface{ Written() bool }); !ok {
				rw = httpx.NewRecorder(rw)
			}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				info := httpx.InfoFrom(r.Context())
				route := routeLabel(info)
				m.Panic(route)

				if logger != nil {
					attrs := []any{
						slog.String("method", logSafe(r.Method, 32)),
						slog.String("path", logSafe(r.URL.Path, 1024)),
						slog.String("route", route),
						slog.String("panic", logSafe(fmt.Sprint(rec), maxPanicLength)),
						slog.String("stack", logSafe(string(debug.Stack()), maxStackLength)),
					}
					if info != nil {
						attrs = append(attrs, slog.String("request_id", info.RequestID))
					}
					logger.Error("panic recovered", attrs...)
				}

				httpx.WriteError(rw, r, httpx.NewError(
					http.StatusInternalServerError,
					httpx.CodeInternalError,
					"Internal server error.",
				))
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
