package ratelimit

import (
	"context"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

func BenchmarkLocalAllow(b *testing.B) {
	limiter := NewLocal(clock.Real{})
	ctx := context.Background()
	rule := Rule{Capacity: 1_000_000, RefillPerSec: 1_000_000, FailOpen: true}
	key := BuildKey("bench", "ip", "", "203.0.113.7")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := limiter.Allow(ctx, key, rule); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLocalAllowManyKeys(b *testing.B) {
	limiter := NewLocal(clock.Real{})
	ctx := context.Background()
	rule := Rule{Capacity: 1_000, RefillPerSec: 1_000, FailOpen: true}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = BuildKey("bench", "ip", "", "10.0.0."+string(rune('0'+i%10)))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := limiter.Allow(ctx, keys[i%len(keys)], rule); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildKey(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if key := BuildKey("users", "header", "X-Tenant", "tenant-42"); key == "" {
			b.Fatal("empty key")
		}
	}
}
