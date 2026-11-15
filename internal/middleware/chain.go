package middleware

import "net/http"

// Chain applies middleware so the first argument runs outermost:
// Chain(h, a, b) serves a(b(h)).
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i](h)
		}
	}
	return h
}
