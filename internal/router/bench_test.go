package router

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func benchmarkRoutes(n int) []config.Route {
	routes := make([]config.Route, 0, n+1)
	for i := 0; i < n; i++ {
		routes = append(routes, config.Route{
			Name:     fmt.Sprintf("route-%02d", i),
			Match:    config.Match{Host: fmt.Sprintf("tenant-%02d.example.com", i), PathPrefix: fmt.Sprintf("/api/v%d", i)},
			Upstream: "bench",
		})
	}
	routes = append(routes, config.Route{
		Name:     "catch-all",
		Match:    config.Match{PathPrefix: "/"},
		Upstream: "bench",
	})
	return routes
}

func BenchmarkRouterMatch(b *testing.B) {
	rt, err := New(benchmarkRoutes(32))
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://tenant-17.example.com/api/v17/users/42?page=2", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		route, err := rt.Match(req)
		if err != nil || route.Config.Name != "route-17" {
			b.Fatalf("match = %v, %v", route, err)
		}
	}
}

func BenchmarkRouterMatchMiss(b *testing.B) {
	routes := benchmarkRoutes(32)
	rt, err := New(routes[:32])
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://other.example.com/unmatched", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Match(req); err == nil {
			b.Fatal("expected no route")
		}
	}
}
