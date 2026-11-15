package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

const testSecret = "test-secret-value"

func hsAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	t.Setenv("GW_TEST_JWT_SECRET", testSecret)
	a, err := New(config.Auth{JWT: config.JWT{
		Algorithms:     []string{"HS256"},
		HS256SecretEnv: "GW_TEST_JWT_SECRET",
		Issuer:         "https://issuer.example",
		Audience:       []string{"gateway"},
		Leeway:         config.Duration{Duration: time.Second},
		ScopesClaim:    "scope",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func signHS(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"sub":   "user-42",
		"iss":   "https://issuer.example",
		"aud":   "gateway",
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"scope": "users:read orders:read",
		"roles": []string{"admin"},
	}
}

func bearerRequest(token string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func requiredJWT() config.RouteAuth {
	return config.RouteAuth{Required: true, Methods: []string{"jwt"}}
}

func TestJWTValid(t *testing.T) {
	a := hsAuthenticator(t)
	p, err := a.Authenticate(bearerRequest(signHS(t, validClaims())), requiredJWT())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "user-42" || p.Kind != "jwt" {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Scopes) != 2 || len(p.Roles) != 1 || p.Roles[0] != "admin" {
		t.Fatalf("scopes/roles = %v %v", p.Scopes, p.Roles)
	}
	if p.LogID() != "user:user-42" {
		t.Fatalf("LogID = %q", p.LogID())
	}
}

func TestJWTScopesListAndClientID(t *testing.T) {
	a := hsAuthenticator(t)
	claims := validClaims()
	delete(claims, "sub")
	delete(claims, "scope")
	claims["client_id"] = "client-1"
	claims["scopes"] = []any{"a", "b", 3}
	p, err := a.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "client-1" || len(p.Scopes) != 2 {
		t.Fatalf("principal = %+v", p)
	}
}

func TestJWTMissingCredentials(t *testing.T) {
	a := hsAuthenticator(t)
	r := httptest.NewRequest("GET", "/", nil)
	_, err := a.Authenticate(r, requiredJWT())
	if reason(err) != ReasonMissing {
		t.Fatalf("error = %v", err)
	}

	p, err := a.Authenticate(r, config.RouteAuth{})
	if err != nil || !p.IsAnonymous() {
		t.Fatalf("optional anonymous = %+v, %v", p, err)
	}
}

func TestJWTExpiredAndNotYetValid(t *testing.T) {
	a := hsAuthenticator(t)
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err := a.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT())
	if reason(err) != ReasonExpired {
		t.Fatalf("expired error = %v (%s)", err, reason(err))
	}

	claims = validClaims()
	claims["nbf"] = time.Now().Add(time.Hour).Unix()
	_, err = a.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT())
	if reason(err) != ReasonNotYetValid {
		t.Fatalf("nbf error = %v (%s)", err, reason(err))
	}
}

func TestJWTClaimsValidation(t *testing.T) {
	a := hsAuthenticator(t)
	tests := []struct {
		name   string
		mutate func(jwt.MapClaims)
		want   Reason
	}{
		{"missing exp", func(c jwt.MapClaims) { delete(c, "exp") }, ReasonInvalid},
		{"wrong issuer", func(c jwt.MapClaims) { c["iss"] = "other" }, ReasonWrongIssuer},
		{"wrong audience", func(c jwt.MapClaims) { c["aud"] = "other" }, ReasonWrongAudience},
		{"missing subject", func(c jwt.MapClaims) { delete(c, "sub") }, ReasonInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims()
			tc.mutate(claims)
			_, err := a.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT())
			if reason(err) != tc.want {
				t.Fatalf("error = %v (%s), want %s", err, reason(err), tc.want)
			}
		})
	}
}

func TestJWTAlgNone(t *testing.T) {
	a := hsAuthenticator(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(bearerRequest(s), requiredJWT()); reason(err) != ReasonInvalid {
		t.Fatalf("alg none accepted: %v", err)
	}
}

func TestJWTWrongSignature(t *testing.T) {
	a := hsAuthenticator(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	s, _ := tok.SignedString([]byte("wrong-secret"))
	if _, err := a.Authenticate(bearerRequest(s), requiredJWT()); reason(err) != ReasonInvalid {
		t.Fatalf("wrong signature error = %v", err)
	}
}

func TestJWTAlgConfusion(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "jwt.pub")
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(config.Auth{JWT: config.JWT{
		Algorithms:         []string{"RS256"},
		RS256PublicKeyFile: pubPath,
		Issuer:             "https://issuer.example",
		Audience:           []string{"gateway"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Sign HS256 using the public PEM as the HMAC secret: a classic confusion
	// attack. The gateway must reject it because HS256 is not enabled.
	pubPEM, _ := os.ReadFile(pubPath)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	s, err := tok.SignedString(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(bearerRequest(s), requiredJWT()); err == nil {
		t.Fatal("algorithm-confusion token was accepted")
	}
}

func TestRS256Valid(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "jwt.pub")
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(config.Auth{JWT: config.JWT{
		Algorithms:         []string{"RS256"},
		RS256PublicKeyFile: pubPath,
		Issuer:             "https://issuer.example",
		Audience:           []string{"gateway"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims())
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Authenticate(bearerRequest(s), requiredJWT())
	if err != nil || p.ID != "user-42" {
		t.Fatalf("principal = %+v, err = %v", p, err)
	}
}

func keyHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func keyAuthenticator(t *testing.T, keys ...config.APIKey) *Authenticator {
	t.Helper()
	a, err := New(config.Auth{APIKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAPIKeyValid(t *testing.T) {
	a := keyAuthenticator(t, config.APIKey{
		ID: "demo", Owner: "owner-1", Hash: keyHash("s3cret"),
		Scopes: []string{"users:read"}, Roles: []string{"service"},
	})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "gw_demo_s3cret")
	p, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "demo" || p.Owner != "owner-1" || p.Kind != "api_key" || p.LogID() != "key:demo" {
		t.Fatalf("principal = %+v", p)
	}
	if p.RateLimitIdentity("user") != "owner-1" || p.RateLimitIdentity("api_key") != "demo" {
		t.Fatalf("rate identities = %q %q", p.RateLimitIdentity("user"), p.RateLimitIdentity("api_key"))
	}

	// Bearer and ApiKey Authorization forms are accepted.
	for _, header := range []string{"Bearer gw_demo_s3cret", "ApiKey gw_demo_s3cret"} {
		r = httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", header)
		if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}}); err != nil {
			t.Fatalf("%q: %v", header, err)
		}
	}
}

func TestAPIKeyFailures(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	a := keyAuthenticator(t,
		config.APIKey{ID: "revoked", Hash: keyHash("s3cret"), Revoked: true},
		config.APIKey{ID: "expired", Hash: keyHash("s3cret"), ExpiresAt: &expired},
		config.APIKey{ID: "valid", Hash: keyHash("s3cret")},
	)
	tests := []struct {
		key  string
		want Reason
	}{
		{"gw_unknown_s3cret", ReasonInvalid},
		{"gw_valid_wrong", ReasonInvalid},
		{"gw_revoked_s3cret", ReasonRevoked},
		{"gw_expired_s3cret", ReasonExpired},
		{"not-a-key", ReasonInvalid},
		{"gw__x", ReasonInvalid},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-API-Key", tc.key)
		_, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}})
		if reason(err) != tc.want {
			t.Errorf("%q error = %v (%s), want %s", tc.key, err, reason(err), tc.want)
		}
	}
}

func TestCredentialTypeNotAllowed(t *testing.T) {
	a := keyAuthenticator(t, config.APIKey{ID: "demo", Hash: keyHash("s3cret")})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "gw_demo_s3cret")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"jwt"}}); reason(err) != ReasonInvalid {
		t.Fatalf("error = %v", err)
	}
}

func TestConflictingCredentials(t *testing.T) {
	a := keyAuthenticator(t, config.APIKey{ID: "demo", Hash: keyHash("s3cret")})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "gw_demo_s3cret")
	r.Header.Set("Authorization", "ApiKey gw_demo_other")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}}); err == nil {
		t.Fatal("conflicting credentials accepted")
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Add("Authorization", "Bearer a")
	r.Header.Add("Authorization", "Bearer b")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}}); err == nil {
		t.Fatal("multiple Authorization headers accepted")
	}
}

func TestAuthorization(t *testing.T) {
	p := &Principal{Kind: "jwt", ID: "u", Scopes: []string{"a", "b"}, Roles: []string{"admin"}}
	if err := Authorize(p, true, []string{"a"}, nil, "any"); err != nil {
		t.Fatal(err)
	}
	if err := Authorize(p, true, []string{"a", "c"}, nil, "any"); err != nil {
		t.Fatal(err)
	}
	if err := Authorize(p, true, []string{"a", "c"}, nil, "all"); err == nil {
		t.Fatal("all should fail")
	}
	if err := Authorize(p, true, nil, []string{"admin"}, "all"); err != nil {
		t.Fatal(err)
	}
	if err := Authorize(p, true, []string{"a"}, []string{"nope"}, "any"); err == nil {
		t.Fatal("role should fail")
	}
	if err := Authorize(nil, true, nil, nil, "any"); reason(err) != ReasonForbidden {
		t.Fatalf("anonymous error = %v", err)
	}
	if err := Authorize(nil, false, nil, nil, "any"); err != nil {
		t.Fatalf("optional should allow anonymous: %v", err)
	}
}

func TestNewErrors(t *testing.T) {
	t.Run("missing env", func(t *testing.T) {
		if _, err := New(config.Auth{JWT: config.JWT{Algorithms: []string{"HS256"}, HS256SecretEnv: "GW_MISSING_SECRET"}}); err == nil {
			t.Fatal("expected missing secret error")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := New(config.Auth{JWT: config.JWT{Algorithms: []string{"RS256"}, RS256PublicKeyFile: "nope.pem"}}); err == nil {
			t.Fatal("expected missing file error")
		}
	})
	t.Run("bad PEM", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.pem")
		_ = os.WriteFile(path, []byte("not a pem"), 0o600)
		if _, err := New(config.Auth{JWT: config.JWT{Algorithms: []string{"RS256"}, RS256PublicKeyFile: path}}); err == nil {
			t.Fatal("expected PEM error")
		}
	})
	t.Run("bad hash", func(t *testing.T) {
		if _, err := New(config.Auth{APIKeys: []config.APIKey{{ID: "x", Hash: "zz"}}}); err == nil {
			t.Fatal("expected hash error")
		}
	})
	t.Run("duplicate key", func(t *testing.T) {
		if _, err := New(config.Auth{APIKeys: []config.APIKey{
			{ID: "x", Hash: keyHash("a")}, {ID: "x", Hash: keyHash("b")},
		}}); err == nil {
			t.Fatal("expected duplicate key error")
		}
	})
}

func TestNoAuthConfigured(t *testing.T) {
	a, err := New(config.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Authenticate(httptest.NewRequest("GET", "/", nil), config.RouteAuth{})
	if err != nil || p.IsAnonymous() == false {
		t.Fatalf("principal = %+v err = %v", p, err)
	}
}

func reason(err error) Reason {
	if err == nil {
		return ""
	}
	if ae, ok := err.(*Error); ok {
		return ae.Reason
	}
	return ReasonInvalid
}

func TestScopeClaimNames(t *testing.T) {
	got := scopeClaimNames("scp")
	if len(got) != 3 || got[0] != "scp" {
		t.Fatalf("scopeClaimNames = %v", got)
	}
	if strings.Join(scopeClaimNames(""), ",") != "scope,scopes" {
		t.Fatalf("default names = %v", scopeClaimNames(""))
	}
}
