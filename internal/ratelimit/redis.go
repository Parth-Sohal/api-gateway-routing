package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketLua string

// redis.NewScript implements EVALSHA with automatic EVAL fallback on NOSCRIPT,
// so an evicted script (or a Redis restart) recovers without a retry loop in
// our code.
var tokenBucketScript = redis.NewScript(tokenBucketLua)

// RedisLimiter checks token buckets stored in Redis.
type RedisLimiter struct {
	client redis.Scripter
}

// NewRedis returns a limiter backed by client.
func NewRedis(client redis.Scripter) *RedisLimiter { return &RedisLimiter{client: client} }

// Allow executes the atomic token-bucket script.
func (l *RedisLimiter) Allow(ctx context.Context, key string, rule Rule) (Decision, error) {
	if l == nil || l.client == nil {
		return Decision{}, fmt.Errorf("redis limiter: no client")
	}
	if err := rule.validate(); err != nil {
		return Decision{}, err
	}
	ttl := int64(math.Ceil(bucketTTL(rule.Capacity, rule.RefillPerSec).Seconds()))
	res, err := tokenBucketScript.Run(ctx, l.client, []string{key},
		rule.Capacity, rule.RefillPerSec, 1, ttl).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("redis rate limit: %w", err)
	}
	if len(res) < 3 {
		return Decision{}, fmt.Errorf("redis rate limit: unexpected script result length %d", len(res))
	}
	allowed, ok := toInt64(res[0])
	if !ok {
		return Decision{}, fmt.Errorf("redis rate limit: unexpected allowed value %T", res[0])
	}
	remaining, ok := toInt64(res[1])
	if !ok {
		return Decision{}, fmt.Errorf("redis rate limit: unexpected remaining value %T", res[1])
	}
	retryMs, _ := toInt64(res[2])
	var resetMs int64
	if len(res) > 3 {
		resetMs, _ = toInt64(res[3])
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > rule.Capacity {
		remaining = rule.Capacity
	}
	return Decision{
		Allowed:    allowed == 1,
		Limit:      rule.Capacity,
		Remaining:  remaining,
		RetryAfter: time.Duration(retryMs) * time.Millisecond,
		ResetAfter: time.Duration(resetMs) * time.Millisecond,
	}, nil
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// bucketTTL is the time to refill an empty bucket plus one second of margin,
// with a hard ceiling to avoid absurd expirations for pathological rules.
func bucketTTL(capacity int64, rate float64) time.Duration {
	secs := math.Ceil(float64(capacity)/rate) + 1
	if secs < 1 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		secs = 1
	}
	const maxTTL = 365 * 24 * time.Hour
	d := time.Duration(secs * float64(time.Second))
	if d > maxTTL {
		d = maxTTL
	}
	return d
}
