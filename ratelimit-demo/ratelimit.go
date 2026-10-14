package main

import (
	"sync"
	"time"
)

// bucket holds the state of one client's token bucket.
type bucket struct {
	tokens     float64   // current number of tokens
	lastRefill time.Time // when we last added tokens
}

// RateLimiter limits requests per client using the token bucket algorithm.
type RateLimiter struct {
	capacity   float64
	refillRate float64 // tokens per second
	buckets    map[string]*bucket
	mu         sync.Mutex
}

// NewRateLimiter creates a limiter with the given capacity and refill rate.
// type of constructor
func NewRateLimiter(capacity, refillRate float64) *RateLimiter {
	return &RateLimiter{
		capacity:   capacity,
		refillRate: refillRate,
		buckets:    make(map[string]*bucket),
	}
}

// Allow returns true if the client is allowed to make one request now.
func (rl *RateLimiter) Allow(clientID string) bool {
	rl.mu.Lock()         // res locked
	defer rl.mu.Unlock() // when func finsihes

	now := time.Now()

	// Look up or create the bucket for this client.
	b, ok := rl.buckets[clientID]
	if !ok {
		// First time we see this client: start with a full bucket.
		b = &bucket{
			tokens:     rl.capacity,
			lastRefill: now,
		}
		rl.buckets[clientID] = b
	}

	// Refill based on how much time has passed.
	elapsed := now.Sub(b.lastRefill).Seconds() // seconds since last refill
	b.tokens += elapsed * rl.refillRate

	// Cap at capacity.
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}

	// Update refill time.
	b.lastRefill = now

	// Try to consume one token.
	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}

	return false
}
