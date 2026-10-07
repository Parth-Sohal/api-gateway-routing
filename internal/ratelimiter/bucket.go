package ratelimiter

import "time"

type Bucket struct {
	tokens     float64
	capacity   float64
	refillRate float64
	lastRefill time.Time
}

func (b *Bucket) refill() {
	now := time.Now()

	elapsed := now.Sub(b.lastRefill).Seconds()

	tokensToAdd := elapsed * b.refillRate

	b.tokens += tokensToAdd

	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}

	b.lastRefill = now
}

func (b *Bucket) allow() bool {
	b.refill()

	if b.tokens >= 1 {
		b.tokens--
		return true
	}

	return false
}