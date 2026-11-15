package httpx

import (
	"net/http"
	"strings"
)

// HopByHopHeaders are connection-specific and must not be forwarded by a
// proxy. Connection-nominated headers are removed separately.
var HopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// IdentityHeaders are set by clients trying to impersonate an authenticated
// principal. The gateway always strips them and sets its own trusted values
// from the authenticated Principal.
var IdentityHeaders = []string{
	"X-User-Id",
	"X-User-Kind",
	"X-User-Scopes",
	"X-User-Roles",
	"X-Principal",
	"X-Authenticated-User",
	"X-Authenticated-Scopes",
	"X-Forwarded-User",
}

// RemoveHopByHop deletes hop-by-hop headers, including any header nominated by
// the Connection header.
func RemoveHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range HopByHopHeaders {
		h.Del(name)
	}
}

// StripIdentityHeaders removes client-controlled identity headers.
func StripIdentityHeaders(h http.Header) {
	for _, name := range IdentityHeaders {
		h.Del(name)
	}
}

// CopyHeader copies all values from src to dst, replacing existing values.
func CopyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// CloneHeader returns a deep copy of h.
func CloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		out[k] = append([]string(nil), vv...)
	}
	return out
}
