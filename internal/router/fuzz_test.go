package router

import (
	"net/http/httptest"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// FuzzMatch asserts that arbitrary host/path/method combinations cannot panic
// route matching and that a successful match always carries a route.
func FuzzMatch(f *testing.F) {
	rt, err := New([]config.Route{
		route("root", "", "/", "GET"),
		route("api", "", "/api"),
		route("users", "example.com", "/api/users", "GET", "POST"),
		route("wild", "*.example.com", "/assets"),
	})
	if err != nil {
		f.Fatal(err)
	}

	f.Add("/api/users/42", "example.com", "GET")
	f.Add("/api/users/42", "other.example.com", "DELETE")
	f.Add("/assets/app.js", "cdn.example.com", "GET")
	f.Add("//../%2e%2e/api", "", "")

	f.Fuzz(func(t *testing.T, path, host, method string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path = path
		r.Host = host
		r.Method = method
		m, err := rt.Match(r)
		if err != nil {
			return
		}
		if m == nil || m.Config == nil {
			t.Fatal("Match returned nil route without an error")
		}
	})
}
