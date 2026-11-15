// Package middleware contains the cross-cutting HTTP layer: request IDs,
// structured access logs with Prometheus metrics, and panic recovery. Order
// matters: RequestID must be outermost so everything downstream can log a
// correlation ID, and Recovery must sit inside AccessLog so the recorded status
// is the 500 envelope rather than the panic's aborted response.
package middleware

import (
	"net/http"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

// HeaderRequestID is the inbound and outbound correlation header.
const HeaderRequestID = httpx.HeaderRequestID

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// RequestID accepts a valid inbound request ID or generates a ULID, records it
// in the response header and attaches request-scoped info (ID and client IP).
func RequestID(trusted httpx.TrustedProxies) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := httpx.SanitizeRequestID(r.Header.Get(HeaderRequestID))
			if !ok {
				id = httpx.NewRequestID()
			}
			w.Header().Set(HeaderRequestID, id)

			info := httpx.InfoFrom(r.Context())
			if info == nil {
				info = &httpx.Info{}
			}
			info.RequestID = id
			if info.ClientIP == "" {
				info.ClientIP = trusted.ClientIP(r)
			}
			r = r.WithContext(httpx.WithInfo(r.Context(), info))
			next.ServeHTTP(w, r)
		})
	}
}
