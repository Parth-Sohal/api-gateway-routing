package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

func TestLocalTokenBucket(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	l := NewLocal(c)
	rule := Rule{Capacity: 3, RefillPerSec: 1, FailOpen: true}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := l.Allow(ctx, "k", rule)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: %+v, %v", i, d, err)
		}
		if d.Remaining != int64(2-i) {
			t.Fatalf("request %d remaining = %d", i, d.Remaining)
		}
	}
	d, err := l.Allow(ctx, "k", rule)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.Remaining != 0 || d.RetryAfter <= 0 {
		t.Fatalf("expected denial, got %+v", d)
	}

	c.Advance(500 * time.Millisecond)
	d, _ = l.Allow(ctx, "k", rule)
	if d.Allowed || d.RetryAfter < 400*time.Millisecond || d.RetryAfter > 600*time.Millisecond {
		t.Fatalf("half refill: %+v", d)
	}

	c.Advance(500 * time.Millisecond)
	d, _ = l.Allow(ctx, "k", rule)
	if !d.Allowed {
		t.Fatalf("should allow after full refill: %+v", d)
	}
	if d.ResetAfter <= 0 {
		t.Fatalf("reset after should be positive: %+v", d)
	}
}

func TestLocalKeyIsolation(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	l := NewLocal(c)
	rule := Rule{Capacity: 1, RefillPerSec: 1}
	if d, _ := l.Allow(context.Background(), "a", rule); !d.Allowed {
		t.Fatal("a first")
	}
	if d, _ := l.Allow(context.Background(), "a", rule); d.Allowed {
		t.Fatal("a should be limited")
	}
	if d, _ := l.Allow(context.Background(), "b", rule); !d.Allowed {
		t.Fatal("b should be independent")
	}
}

func TestLocalConcurrentAccuracy(t *testing.T) {
	l := NewLocal(clock.NewFake(time.Unix(1000, 0)))
	rule := Rule{Capacity: 100, RefillPerSec: 0.000001}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := l.Allow(context.Background(), "shared", rule); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 100 {
		t.Fatalf("allowed = %d, want exactly 100", got)
	}
}

func TestLocalRuleChangeResets(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	l := NewLocal(c)
	ctx := context.Background()
	rule := Rule{Capacity: 1, RefillPerSec: 1}
	if d, _ := l.Allow(ctx, "k", rule); !d.Allowed {
		t.Fatal("first")
	}
	if d, _ := l.Allow(ctx, "k", rule); d.Allowed {
		t.Fatal("second should be limited")
	}
	rule.Capacity = 5
	if d, _ := l.Allow(ctx, "k", rule); !d.Allowed || d.Remaining != 4 {
		t.Fatalf("rule change should reset bucket: %+v", d)
	}
}

func TestLocalJanitor(t *testing.T) {
	c := clock.NewFake(time.Unix(1000, 0))
	l := NewLocal(c)
	rule := Rule{Capacity: 1, RefillPerSec: 1}
	for _, key := range []string{"a", "b", "c"} {
		_, _ = l.Allow(context.Background(), key, rule)
	}
	if l.bucketCount() != 3 {
		t.Fatalf("bucket count = %d", l.bucketCount())
	}
	c.Advance(10 * time.Minute)
	l.sweep(c.Now())
	if l.bucketCount() != 0 {
		t.Fatalf("buckets not swept: %d", l.bucketCount())
	}

	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)
	l.Start(ctx)
	cancel()
}

func TestLocalInvalidRule(t *testing.T) {
	l := NewLocal(nil)
	for _, rule := range []Rule{{Capacity: 0, RefillPerSec: 1}, {Capacity: 1, RefillPerSec: 0}} {
		if _, err := l.Allow(context.Background(), "k", rule); err == nil {
			t.Fatalf("expected error for %+v", rule)
		}
	}
}

func TestShardIndex(t *testing.T) {
	seen := make(map[int]struct{})
	for i := 0; i < 2000; i++ {
		seen[shardIndex(string(rune('a'+i%26))+string(rune(i)))] = struct{}{}
	}
	if len(seen) < localShards/2 {
		t.Fatalf("only %d shards used", len(seen))
	}
}

func TestBuildKey(t *testing.T) {
	if got := BuildKey("users", "ip", "", "203.0.113.9"); got != "rl:users:ip:203.0.113.9" {
		t.Fatalf("BuildKey = %q", got)
	}
	if got := BuildKey("users", "header", "X-Tenant", "acme"); got != "rl:users:header:x-tenant:acme" {
		t.Fatalf("header key = %q", got)
	}
	if got := BuildKey("", "", "", ""); got != "rl:unknown:ip:unknown" {
		t.Fatalf("empty key = %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := BuildKey("r", "user", "", long)
	if len(got) > 160 || !contains(got, ":h:") {
		t.Fatalf("long identity not hashed: %q", got)
	}
	if got := BuildKey("route with spaces!", "ip", "", "bad identity\n"); !contains(got, ":h:") {
		t.Fatalf("unsafe identity not hashed: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
