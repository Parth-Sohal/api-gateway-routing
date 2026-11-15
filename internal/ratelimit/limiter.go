// Package ratelimit implements a distributed token bucket backed by Redis and
// a sharded in-memory fallback. The Redis path executes a single Lua script so
// read-modify-write is atomic across gateway instances.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Rule is a token-bucket rule. Capacity is the maximum burst; RefillPerSec is
// the sustained rate. FailOpen controls the Redis-outage policy.
type Rule struct {
	Capacity     int64
	RefillPerSec float64
	FailOpen     bool
}

func (r Rule) validate() error {
	if r.Capacity < 1 {
		return fmt.Errorf("capacity must be at least 1")
	}
	if r.RefillPerSec <= 0 || math.IsNaN(r.RefillPerSec) || math.IsInf(r.RefillPerSec, 0) {
		return fmt.Errorf("refill_per_sec must be a positive finite number")
	}
	return nil
}

// Decision is the outcome of one rate-limit check.
type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	RetryAfter time.Duration
	ResetAfter time.Duration
	// Degraded is true when the answer came from the local fallback because
	// Redis was unavailable.
	Degraded bool
}

// Limiter checks one rate-limit key.
type Limiter interface {
	Allow(ctx context.Context, key string, rule Rule) (Decision, error)
}

// ErrUnavailable indicates that the limiter could not make a decision and the
// route's fail_open policy is false.
var ErrUnavailable = errors.New("rate limiter unavailable")

// BuildKey creates a bounded, structured Redis key:
//
//	rl:<route>:<key_type>:<identity>
//
// Route names bound the key space; arbitrary header values are hashed so keys
// cannot grow without bound.
func BuildKey(route, keyType, header, identity string) string {
	route = safePart(route, 48)
	if route == "" {
		route = "unknown"
	}
	keyType = strings.ToLower(strings.TrimSpace(keyType))
	if keyType == "" {
		keyType = "ip"
	}
	if keyType == "header" {
		h := safePart(strings.ToLower(strings.TrimSpace(header)), 48)
		if h == "" {
			h = "missing"
		}
		keyType = "header:" + h
	}
	id := safeIdentity(identity)
	return "rl:" + route + ":" + keyType + ":" + id
}

func safePart(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':' || c == '@' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func safeIdentity(s string) string {
	if s == "" {
		return "unknown"
	}
	if len(s) <= 96 && safePart(s, len(s)) == s {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "h:" + hex.EncodeToString(sum[:16])
}
