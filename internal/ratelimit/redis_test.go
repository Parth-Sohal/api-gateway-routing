package ratelimit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client, *RedisLimiter) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client, NewRedis(client)
}

func TestRedisTokenBucket(t *testing.T) {
	_, _, limiter := newTestRedis(t)
	ctx := context.Background()
	rule := Rule{Capacity: 2, RefillPerSec: 1, FailOpen: true}
	for i := 0; i < 2; i++ {
		d, err := limiter.Allow(ctx, "k", rule)
		if err != nil || !d.Allowed || d.Limit != 2 {
			t.Fatalf("request %d: %+v, %v", i, d, err)
		}
	}
	d, err := limiter.Allow(ctx, "k", rule)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.RetryAfter <= 0 || d.Remaining != 0 {
		t.Fatalf("expected denial: %+v", d)
	}
}

func TestRedisAtomicityAcrossInstances(t *testing.T) {
	_, client, _ := newTestRedis(t)
	limiters := []*RedisLimiter{NewRedis(client), NewRedis(client), NewRedis(client)}
	const capacity = 100
	rule := Rule{Capacity: capacity, RefillPerSec: 0.000001, FailOpen: true}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		l := limiters[i%len(limiters)]
		go func() {
			defer wg.Done()
			if d, err := l.Allow(context.Background(), "shared", rule); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != capacity {
		t.Fatalf("allowed = %d, want exactly %d", got, capacity)
	}
}

func TestRedisRefillAndTTL(t *testing.T) {
	mr, _, limiter := newTestRedis(t)
	ctx := context.Background()
	rule := Rule{Capacity: 1, RefillPerSec: 1}
	if d, _ := limiter.Allow(ctx, "k", rule); !d.Allowed {
		t.Fatal("first should pass")
	}
	if d, _ := limiter.Allow(ctx, "k", rule); d.Allowed {
		t.Fatal("second should be limited")
	}
	if ttl := mr.TTL("k"); ttl <= 0 {
		t.Fatalf("TTL = %v", ttl)
	}
	// miniredis's TIME uses its settable clock; FastForward only expires TTLs.
	mr.SetTime(time.Now().Add(2 * time.Second))
	if d, err := limiter.Allow(ctx, "k", rule); err != nil || !d.Allowed {
		t.Fatalf("after refill: %+v, %v", d, err)
	}
}

func TestRedisKeyIsolation(t *testing.T) {
	_, _, limiter := newTestRedis(t)
	ctx := context.Background()
	rule := Rule{Capacity: 1, RefillPerSec: 0.001}
	if d, _ := limiter.Allow(ctx, "a", rule); !d.Allowed {
		t.Fatal("a")
	}
	if d, _ := limiter.Allow(ctx, "a", rule); d.Allowed {
		t.Fatal("a limited")
	}
	if d, _ := limiter.Allow(ctx, "b", rule); !d.Allowed {
		t.Fatal("b")
	}
}

func TestRedisNOSCRIPTRecovery(t *testing.T) {
	mr, client, limiter := newTestRedis(t)
	ctx := context.Background()
	rule := Rule{Capacity: 5, RefillPerSec: 1}
	if d, err := limiter.Allow(ctx, "k", rule); err != nil || !d.Allowed {
		t.Fatalf("first: %+v %v", d, err)
	}
	if err := client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	// miniredis does not implement SCRIPT FLUSH fully; restarting the server
	// clears the script cache in a real deployment. This call exercises the
	// normal EVALSHA path after a flush where available.
	if d, err := limiter.Allow(ctx, "k", rule); err != nil || !d.Allowed {
		t.Fatalf("after flush: %+v %v", d, err)
	}
	_ = mr
}

func TestRedisInvalidRule(t *testing.T) {
	_, _, limiter := newTestRedis(t)
	if _, err := limiter.Allow(context.Background(), "k", Rule{}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRedisUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
		MaxRetries:   -1,
	})
	defer client.Close()
	limiter := NewRedis(client)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := limiter.Allow(ctx, "k", Rule{Capacity: 1, RefillPerSec: 1})
	if err == nil {
		t.Fatal("expected connection error")
	}
}

type stubLimiter struct {
	decision Decision
	err      error
	wait     bool
}

func (s *stubLimiter) Allow(ctx context.Context, _ string, _ Rule) (Decision, error) {
	if s.wait {
		<-ctx.Done()
		return Decision{}, ctx.Err()
	}
	return s.decision, s.err
}

func TestFallbackFailOpen(t *testing.T) {
	local := NewLocal(nil)
	var fallbacks atomic.Int64
	var reasonsMu sync.Mutex
	var reasons []string
	primary := &stubLimiter{err: errors.New("redis down")}
	l := NewFallback(FallbackOptions{
		Primary:    primary,
		Local:      local,
		Timeout:    20 * time.Millisecond,
		OnError:    func(reason string) { reasonsMu.Lock(); reasons = append(reasons, reason); reasonsMu.Unlock() },
		OnFallback: func() { fallbacks.Add(1) },
	})
	d, err := l.Allow(context.Background(), "k", Rule{Capacity: 2, RefillPerSec: 1, FailOpen: true})
	if err != nil || !d.Allowed || !d.Degraded {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	if fallbacks.Load() != 1 {
		t.Fatalf("fallback count = %d", fallbacks.Load())
	}
	reasonsMu.Lock()
	got := strings.Join(reasons, ",")
	reasonsMu.Unlock()
	if got != "error" {
		t.Fatalf("reasons = %q", got)
	}
}

func TestFallbackFailClosed(t *testing.T) {
	l := NewFallback(FallbackOptions{
		Primary: &stubLimiter{err: errors.New("redis down")},
		Local:   NewLocal(nil),
		Timeout: 20 * time.Millisecond,
	})
	_, err := l.Allow(context.Background(), "k", Rule{Capacity: 2, RefillPerSec: 1, FailOpen: false})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestFallbackTimeoutReason(t *testing.T) {
	var reason atomic.Value
	l := NewFallback(FallbackOptions{
		Primary: &stubLimiter{wait: true},
		Local:   NewLocal(nil),
		Timeout: 10 * time.Millisecond,
		OnError: func(r string) { reason.Store(r) },
	})
	d, err := l.Allow(context.Background(), "k", Rule{Capacity: 1, RefillPerSec: 1, FailOpen: true})
	if err != nil || !d.Allowed {
		t.Fatalf("decision = %+v err = %v", d, err)
	}
	if got, _ := reason.Load().(string); got != "timeout" {
		t.Fatalf("reason = %q", got)
	}
}

func TestFallbackCancelledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := NewFallback(FallbackOptions{Primary: &stubLimiter{err: errors.New("x")}, Local: NewLocal(nil)})
	if _, err := l.Allow(ctx, "k", Rule{Capacity: 1, RefillPerSec: 1, FailOpen: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestFallbackNilPrimary(t *testing.T) {
	l := NewFallback(FallbackOptions{Local: NewLocal(nil)})
	d, err := l.Allow(context.Background(), "k", Rule{Capacity: 1, RefillPerSec: 1, FailOpen: true})
	if err != nil || !d.Allowed || !d.Degraded {
		t.Fatalf("decision = %+v err = %v", d, err)
	}
}
