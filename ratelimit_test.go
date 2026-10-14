package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func newTestLimiter(t *testing.T, capacity, refillRate float64) *RateLimiter {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx := context.Background()
	rdb.FlushDB(ctx) // clear any old state

	t.Cleanup(func() { rdb.Close() })
	return NewRateLimiter(rdb, capacity, refillRate)
}

func TestTokenBucketBurst(t *testing.T) {
	rl := newTestLimiter(t, 3, 1)

	// First 3 should be allowed
	for i := 1; i <= 3; i++ {
		if !rl.Allow("client-A") {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// Next 2 should be denied
	for i := 4; i <= 5; i++ {
		if rl.Allow("client-A") {
			t.Fatalf("request %d should be denied", i)
		}
	}
}

func TestTokenBucketRefill(t *testing.T) {
	rl := newTestLimiter(t, 3, 1)

	// Drain the bucket
	for range 3 {
		rl.Allow("client-A")
	}

	// Immediately after, should be denied
	if rl.Allow("client-A") {
		t.Fatal("should be denied right after draining")
	}

	// Wait 2 seconds → 2 tokens should refill
	time.Sleep(2 * time.Second)

	if !rl.Allow("client-A") {
		t.Fatal("should be allowed after refill")
	}
	if !rl.Allow("client-A") {
		t.Fatal("second request should also be allowed")
	}
	if rl.Allow("client-A") {
		t.Fatal("third request should be denied (only 2 refilled)")
	}
}

func TestDifferentClientsHaveSeparateBuckets(t *testing.T) {
	rl := newTestLimiter(t, 2, 1)

	// Drain client-A
	rl.Allow("client-A")
	rl.Allow("client-A")
	if rl.Allow("client-A") {
		t.Fatal("client-A should be denied")
	}

	// client-B should still have a full bucket
	if !rl.Allow("client-B") {
		t.Fatal("client-B should be allowed (separate bucket)")
	}
}

// concurrency test allowed 100 -> but if 200 arrive
func TestConcurrentAccess(t *testing.T) {
	rl := newTestLimiter(t, 100, 0.0001) // capacity 100, essentially no refill

	var wg sync.WaitGroup
	var allowed int64
	var denied int64

	// 200 goroutines all try at once
	for range 200 {
		wg.Go(func() {
			if rl.Allow("same-client") {
				atomic.AddInt64(&allowed, 1)
			} else {
				atomic.AddInt64(&denied, 1)
			}
		})
	}
	wg.Wait()

	t.Logf("allowed=%d denied=%d", allowed, denied)

	// Exactly 100 should be allowed, 100 denied
	if allowed != 100 {
		t.Fatalf("expected exactly 100 allowed, got %d", allowed)
	}
}
