package ratelimiter

import (
	"sync"
	"time"
)

type RateLimiter struct {
	bucket *Bucket
	mu     sync.Mutex
}

func NewRateLimiter(capacity float64, refillRate float64) *RateLimiter {
	bucket := &Bucket{
		tokens:     capacity,
		capacity:   capacity,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}

	return &RateLimiter{
		bucket: bucket,
	}
}

func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.bucket.allow()
}