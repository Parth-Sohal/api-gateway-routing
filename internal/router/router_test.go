package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func route(name, host, prefix string, methods ...string) config.Route {
	return config.Route{
		Name:     name,
		Upstream: "upstream",
		Match: config.Match{
			Host:       host,
			PathPrefix: prefix,
			Methods:    methods,
		},
	}
}

func mustRouter(t *testing.T, routes ...config.Route) *Router {
	t.Helper()
	rt, err := New(routes)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func request(method, target, host string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	return r
}

func TestLongestPrefixWins(t *testing.T) {
	rt := mustRouter(t,
		route("api", "", "/api"),
		route("users", "", "/api/users"),
		route("admin", "", "/api/users/admin"),
	)
	got, err := rt.Match(request("GET", "/api/users/42", "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Name != "users" {
		t.Fatalf("route = %q, want users", got.Config.Name)
	}
	got, _ = rt.Match(request("GET", "/api/users/admin/x", "example.com"))
	if got.Config.Name != "admin" {
		t.Fatalf("route = %q, want admin", got.Config.Name)
	}
}

func TestPrefixBoundary(t *testing.T) {
	rt := mustRouter(t, route("api", "", "/api"))
	for _, path := range []string{"/api", "/api/", "/api/users"} {
		if _, err := rt.Match(request("GET", path, "x")); err != nil {
			t.Errorf("%s should match: %v", path, err)
		}
	}
	for _, path := range []string{"/apix", "/apiary/users", "/"} {
		_, err := rt.Match(request("GET", path, "x"))
		if err == nil {
			t.Errorf("%s should not match", path)
		}
		if me, ok := err.(*MatchError); !ok || me.Status != http.StatusNotFound {
			t.Errorf("%s error = %v", path, err)
		}
	}
}

func TestHostSpecificity(t *testing.T) {
	rt := mustRouter(t,
		route("any", "", "/api"),
		route("wildcard", "*.example.com", "/api"),
		route("exact", "api.example.com", "/api"),
	)
	cases := []struct {
		host string
		want string
	}{
		{"api.example.com", "exact"},
		{"API.EXAMPLE.COM:8443", "exact"},
		{"www.example.com", "wildcard"},
		{"example.com", "any"},
		{"evil-api.example.com.attacker.net", "any"},
	}
	for _, tc := range cases {
		got, err := rt.Match(request("GET", "/api", tc.host))
		if err != nil {
			t.Errorf("host %q: %v", tc.host, err)
			continue
		}
		if got.Config.Name != tc.want {
			t.Errorf("host %q route = %q, want %q", tc.host, got.Config.Name, tc.want)
		}
	}
}

func TestWildcardDoesNotMatchApex(t *testing.T) {
	rt := mustRouter(t, route("wildcard", "*.example.com", "/api"))
	if _, err := rt.Match(request("GET", "/api", "example.com")); err == nil {
		t.Fatal("wildcard should not match apex")
	}
}

func TestLongestPrefixBeforeHost(t *testing.T) {
	// The documented precedence is longest prefix, then host specificity.
	rt := mustRouter(t,
		route("exact-short", "api.example.com", "/api"),
		route("wildcard-long", "*.example.com", "/api/users"),
	)
	got, err := rt.Match(request("GET", "/api/users", "api.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Name != "wildcard-long" {
		t.Fatalf("route = %q, want wildcard-long", got.Config.Name)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rt := mustRouter(t,
		route("get", "", "/api", "GET"),
		route("post", "", "/api", "POST", "PUT"),
	)
	_, err := rt.Match(request("DELETE", "/api", "x"))
	me, ok := err.(*MatchError)
	if !ok || me.Status != http.StatusMethodNotAllowed {
		t.Fatalf("error = %v", err)
	}
	want := []string{"GET", "POST", "PUT"}
	if len(me.Allowed) != len(want) {
		t.Fatalf("allowed = %v", me.Allowed)
	}
	for i := range want {
		if me.Allowed[i] != want[i] {
			t.Fatalf("allowed = %v, want %v", me.Allowed, want)
		}
	}

	got, err := rt.Match(request("PUT", "/api", "x"))
	if err != nil || got.Config.Name != "post" {
		t.Fatalf("PUT route = %v, %v", got, err)
	}
}

func TestExactHostMethodDoesNotFallBack(t *testing.T) {
	rt := mustRouter(t,
		route("exact-get", "api.example.com", "/api", "GET"),
		route("wildcard-post", "*.example.com", "/api", "POST"),
	)
	_, err := rt.Match(request("POST", "/api", "api.example.com"))
	me, ok := err.(*MatchError)
	if !ok || me.Status != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on exact host, got %v", err)
	}
}

func TestAnyMethodRoute(t *testing.T) {
	rt := mustRouter(t, route("any", "", "/"))
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		if _, err := rt.Match(request(method, "/anything", "x")); err != nil {
			t.Errorf("%s: %v", method, err)
		}
	}
}

func TestNoRoute(t *testing.T) {
	rt := mustRouter(t, route("api", "", "/api"))
	_, err := rt.Match(request("GET", "/other", "x"))
	me, ok := err.(*MatchError)
	if !ok || me.Status != http.StatusNotFound {
		t.Fatalf("error = %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New([]config.Route{{Name: "", Match: config.Match{PathPrefix: "/"}}}); err == nil {
		t.Fatal("expected empty name error")
	}
	if _, err := New([]config.Route{{Name: "x", Match: config.Match{PathPrefix: "no-slash"}}}); err == nil {
		t.Fatal("expected prefix error")
	}
}

func TestAllowedMethods(t *testing.T) {
	r := route("x", "", "/", "POST", "GET")
	rt := mustRouter(t, r)
	got := rt.Routes()[0].AllowedMethods()
	if len(got) != 2 || got[0] != "GET" || got[1] != "POST" {
		t.Fatalf("AllowedMethods = %v", got)
	}
	all := route("all", "", "/")
	rt = mustRouter(t, all)
	if got := rt.Routes()[0].AllowedMethods(); got != nil {
		t.Fatalf("all-method route returned %v", got)
	}
}
