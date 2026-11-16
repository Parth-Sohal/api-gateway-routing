package httpx

import (
	"net/http/httptest"
	"testing"
)

// FuzzClientIP asserts that malformed peer addresses and forwarded chains
// cannot panic the trust-aware client IP resolver.
func FuzzClientIP(f *testing.F) {
	f.Add("192.0.2.10:443", "203.0.113.9, 10.0.0.1")
	f.Add("10.0.0.1:1234", "unknown")
	f.Add("", "")
	f.Add("[::1]:8080", "2001:db8::1")

	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "::1"})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, remote, forwarded string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		got := ClientIP(r, trusted)
		if got == "" {
			t.Fatal("ClientIP returned an empty address")
		}
	})
}
