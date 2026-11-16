package breaker

import (
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/clock"
)

func BenchmarkAllowSuccess(b *testing.B) {
	bkr := New(Config{
		FailureThreshold:  5,
		FailureRatio:      0.5,
		MinRequests:       20,
		Window:            time.Second,
		Buckets:           10,
		OpenTimeout:       time.Second,
		HalfOpenMaxProbes: 3,
		HalfOpenSuccesses: 2,
	}, clock.Real{}, Hooks{})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done, err := bkr.Allow()
		if err != nil {
			b.Fatal(err)
		}
		done(nil)
	}
}

func BenchmarkAllowParallel(b *testing.B) {
	bkr := New(Config{}, clock.Real{}, Hooks{})

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			done, err := bkr.Allow()
			if err != nil {
				b.Error(err)
				return
			}
			done(nil)
		}
	})
}
