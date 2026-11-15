package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "/", true},
		{"/", "/", true},
		{"/api/users", "/api/users", true},
		{"//api///users", "/api/users", true},
		{"/api/../admin", "/admin", true},
		{"/api/%2e%2e/admin", "/api/%2e%2e/admin", true}, // URL.Path is already decoded by net/http
		{"api", "", false},
		{"/bad\x00path", "", false},
	}
	for _, tc := range tests {
		got, err := NormalizePath(tc.in)
		if tc.ok && err != nil {
			t.Errorf("NormalizePath(%q) error: %v", tc.in, err)
			continue
		}
		if !tc.ok && err == nil {
			t.Errorf("NormalizePath(%q) expected error", tc.in)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func mustProxies(t *testing.T, values ...string) TrustedProxies {
	t.Helper()
	tp, err := ParseTrustedProxies(values)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		xff     string
		trusted TrustedProxies
		want    string
	}{
		{"direct", "203.0.113.9:5555", "", TrustedProxies{}, "203.0.113.9"},
		{"untrusted spoof", "198.51.100.4:1", "1.2.3.4", TrustedProxies{}, "198.51.100.4"},
		{"trusted rightmost", "10.0.0.5:1", "198.51.100.7, 10.0.0.5", mustProxies(t, "10.0.0.0/8"), "198.51.100.7"},
		{"trusted chain", "10.0.0.5:1", "203.0.113.1, 10.0.0.4", mustProxies(t, "10.0.0.0/8"), "203.0.113.1"},
		{"all trusted", "10.0.0.5:1", "10.0.0.1, 10.0.0.2", mustProxies(t, "10.0.0.0/8"), "10.0.0.1"},
		{"ports", "10.0.0.5:1", "198.51.100.7:1234", mustProxies(t, "10.0.0.0/8"), "198.51.100.7"},
		{"ipv6", "[2001:db8::1]:443", "", TrustedProxies{}, "2001:db8::1"},
		{"ipv6 xff", "[::1]:443", "2001:db8::2", mustProxies(t, "::1/128"), "2001:db8::2"},
		{"malformed entries", "10.0.0.5:1", "not-an-ip, 198.51.100.7", mustProxies(t, "10.0.0.0/8"), "198.51.100.7"},
		{"bare remote", "203.0.113.9", "", TrustedProxies{}, "203.0.113.9"},
		{"unknown", "", "", TrustedProxies{}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := tc.trusted.ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	tp := mustProxies(t, "10.0.0.0/8", "192.0.2.10", "::1")
	if !tp.Contains(parseIP("10.1.2.3")) || !tp.Contains(parseIP("192.0.2.10")) || !tp.Contains(parseIP("::1")) {
		t.Fatal("expected addresses to be trusted")
	}
	if tp.Contains(parseIP("203.0.113.1")) || tp.Contains(nil) {
		t.Fatal("unexpected trust")
	}
	for _, bad := range []string{"nope", "10.0.0.0/99", "999.1.1.1"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom, keep-alive")
	h.Set("X-Custom", "remove-me")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Upgrade", "websocket")
	h.Set("X-Keep", "yes")
	RemoveHopByHop(h)
	for _, name := range []string{"Connection", "X-Custom", "Keep-Alive", "Upgrade"} {
		if h.Get(name) != "" {
			t.Errorf("%s should have been removed", name)
		}
	}
	if h.Get("X-Keep") != "yes" {
		t.Error("end-to-end header was removed")
	}
}

func TestStripIdentityHeaders(t *testing.T) {
	h := http.Header{}
	for _, name := range IdentityHeaders {
		h.Set(name, "spoof")
	}
	h.Set("X-Other", "keep")
	StripIdentityHeaders(h)
	for _, name := range IdentityHeaders {
		if h.Get(name) != "" {
			t.Errorf("%s should have been stripped", name)
		}
	}
	if h.Get("X-Other") != "keep" {
		t.Error("unrelated header removed")
	}
}

func TestCloneHeader(t *testing.T) {
	h := http.Header{"X-Test": {"a", "b"}}
	c := CloneHeader(h)
	c.Set("X-Test", "changed")
	c.Add("X-Test", "more")
	if len(h["X-Test"]) != 2 || h.Get("X-Test") != "a" {
		t.Fatalf("clone mutated source: %v", h)
	}
}
