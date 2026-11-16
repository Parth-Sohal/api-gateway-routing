package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func benchmarkAuthenticator(b *testing.B) (*Authenticator, *http.Request) {
	b.Helper()
	const envName = "GW_BENCH_JWT_SECRET"
	if err := os.Setenv(envName, testSecret); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.Unsetenv(envName) })
	a, err := New(config.Auth{
		JWT: config.JWT{
			Algorithms:     []string{"HS256"},
			HS256SecretEnv: envName,
			Issuer:         "https://issuer.example",
			Audience:       []string{"gateway"},
		},
		APIKeys: []config.APIKey{{ID: "demo", Owner: "demo", Hash: keyHash("s3cret")}},
	})
	if err != nil {
		b.Fatal(err)
	}
	return a, httptest.NewRequest("GET", "/", nil)
}

func BenchmarkAuthenticateJWT(b *testing.B) {
	a, req := benchmarkAuthenticator(b)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	signed, err := token.SignedString([]byte(testSecret))
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	policy := config.RouteAuth{Required: true, Methods: []string{"jwt"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Authenticate(req, policy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthenticateAPIKey(b *testing.B) {
	a, req := benchmarkAuthenticator(b)
	req.Header.Set("X-API-Key", "gw_demo_s3cret")
	policy := config.RouteAuth{Required: true, Methods: []string{"api_key"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Authenticate(req, policy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthorize(b *testing.B) {
	p := &Principal{Kind: "jwt", ID: "u", Scopes: []string{"users:read", "orders:read"}, Roles: []string{"developer"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Authorize(p, true, []string{"users:read"}, []string{"developer"}, "any"); err != nil {
			b.Fatal(err)
		}
	}
}
