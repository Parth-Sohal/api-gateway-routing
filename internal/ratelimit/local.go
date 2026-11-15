package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

const localShards = 64

// LocalLimiter is a sharded in-memory token-bucket store used when Redis is
// unavailable and as a fast unit-testable implementation.
type LocalLimiter struct {
	shards [localShards]localShard
	clock  clock.Clock
	once   sync.Once
}

type localShard struct {
	mu      sync.Mutex
	buckets map[string]*localBucket
}

type localBucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
	capacity int64
	rate     float64
	ttl      time.Duration
}

// NewLocal creates a local limiter. A nil clock uses time.Now.
func NewLocal(c clock.Clock) *LocalLimiter {
	if c == nil {
		c = clock.Real{}
	}
	return &LocalLimiter{clock: c}
}

// Start launches the janitor that expires idle buckets. It is safe to call
// more than once; the goroutine exits when ctx is cancelled.
func (l *LocalLimiter) Start(ctx context.Context) {
	l.once.Do(func() {
		go l.janitor(ctx)
	})
}

// Allow implements Limiter.
func (l *LocalLimiter) Allow(_ context.Context, key string, rule Rule) (Decision, error) {
	if err := rule.validate(); err != nil {
		return Decision{}, err
	}
	idx := shardIndex(key)
	shard := &l.shards[idx]
	now := l.clock.Now()

	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.buckets == nil {
		shard.buckets = make(map[string]*localBucket)
	}
	b, ok := shard.buckets[key]
	if !ok || b.capacity != rule.Capacity || b.rate != rule.RefillPerSec {
		b = &localBucket{tokens: float64(rule.Capacity), last: now, capacity: rule.Capacity, rate: rule.RefillPerSec}
		shard.buckets[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens = math.Min(float64(b.capacity), b.tokens+elapsed*b.rate)
	if b.tokens < 0 {
		b.tokens = 0
	}

	d := Decision{Limit: rule.Capacity}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
	}
	if b.tokens < float64(rule.Capacity) {
		d.ResetAfter = time.Duration((float64(rule.Capacity) - b.tokens) / b.rate * float64(time.Second))
	}
	d.Remaining = int64(math.Floor(b.tokens))
	if d.Remaining < 0 {
		d.Remaining = 0
	}
	if d.Remaining > rule.Capacity {
		d.Remaining = rule.Capacity
	}
	if d.RetryAfter < 0 {
		d.RetryAfter = 0
	}
	if d.ResetAfter < 0 {
		d.ResetAfter = 0
	}

	b.last = now
	b.lastSeen = now
	b.ttl = bucketTTL(rule.Capacity, rule.RefillPerSec)
	return d, nil
}

func (l *LocalLimiter) janitor(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.sweep(l.clock.Now())
		}
	}
}

// sweep removes buckets that have been idle longer than their refill TTL.
func (l *LocalLimiter) sweep(now time.Time) {
	for i := range l.shards {
		shard := &l.shards[i]
		shard.mu.Lock()
		for key, b := range shard.buckets {
			ttl := b.ttl
			if ttl <= 0 {
				ttl = time.Minute
			}
			if now.Sub(b.lastSeen) > ttl {
				delete(shard.buckets, key)
			}
		}
		shard.mu.Unlock()
	}
}

func (l *LocalLimiter) bucketCount() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].buckets)
		l.shards[i].mu.Unlock()
	}
	return n
}

func shardIndex(key string) int {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	var h uint64 = offset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	return int(h % localShards)
}
