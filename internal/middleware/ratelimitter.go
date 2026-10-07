package middleware

import (
	"net/http"

	"api-gateway-routing/internal/ratelimiter"
)

func RateLimit(limiter *ratelimiter.RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if !limiter.Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

