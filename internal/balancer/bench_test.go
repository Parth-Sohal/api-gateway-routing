package balancer

import (
	"fmt"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

type benchHealth struct{}

func (benchHealth) Available(string) bool { return true }

func benchmarkTargets(n int) []config.Target {
	targets := make([]config.Target, n)
	for i := range targets {
		targets[i] = config.Target{URL: fmt.Sprintf("http://10.0.0.%d:9001", i+1), Weight: i%3 + 1}
	}
	return targets
}

func BenchmarkAcquireRoundRobin(b *testing.B) {
	benchmarkAcquire(b, string(RoundRobin))
}

func BenchmarkAcquireWeightedRoundRobin(b *testing.B) {
	benchmarkAcquire(b, string(WeightedRoundRobin))
}

func BenchmarkAcquireLeastConn(b *testing.B) {
	benchmarkAcquire(b, string(LeastConn))
}

func benchmarkAcquire(b *testing.B, strategy string) {
	pool, err := New("bench", strategy, benchmarkTargets(16), benchHealth{})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target, err := pool.Acquire(nil)
		if err != nil {
			b.Fatal(err)
		}
		pool.Release(target)
	}
}
