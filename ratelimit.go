package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript is the atomic rate limiting logic.
// Runs entirely inside Redis — no other Redis command can interleave.
var tokenBucketScript = redis.NewScript(`
local key         = KEYS[1]
local capacity    = tonumber(ARGV[1])
local refillRate  = tonumber(ARGV[2])
local nowNano     = tonumber(ARGV[3])

local tokensStr      = redis.call("HGET", key, "tokens")
local lastRefillStr  = redis.call("HGET", key, "lastRefill")

local tokens
local lastRefillNano

if tokensStr == false then
    tokens = capacity
    lastRefillNano = nowNano
else
    tokens = tonumber(tokensStr)
    lastRefillNano = tonumber(lastRefillStr)
end

local elapsedSeconds = (nowNano - lastRefillNano) / 1000000000
tokens = tokens + (elapsedSeconds * refillRate)

if tokens > capacity then
    tokens = capacity
end

local allowed = 0
if tokens >= 1 then
    tokens = tokens - 1
    allowed = 1
end

redis.call("HSET", key, "tokens", tokens)
redis.call("HSET", key, "lastRefill", nowNano)
redis.call("EXPIRE", key, 3600)

return allowed
`)

type RateLimiter struct {
	rdb        *redis.Client
	capacity   float64
	refillRate float64
}

func NewRateLimiter(rdb *redis.Client, capacity, refillRate float64) *RateLimiter {
	return &RateLimiter{
		rdb:        rdb,
		capacity:   capacity,
		refillRate: refillRate,
	}
}

func (rl *RateLimiter) Allow(clientID string) bool {

	ctx, cancel := context.WithTimeout(
		context.Background(),
		3*time.Second,
	)

	defer cancel()

	key := "ratelimit:" + clientID
	nowNano := time.Now().UnixNano()

	result, err := tokenBucketScript.Run(
		ctx,
		rl.rdb,
		[]string{key},
		rl.capacity,
		rl.refillRate,
		nowNano,
	).Int()

	if err != nil {
		// Fail open if Redis is broken
		return true
	}

	return result == 1
}
