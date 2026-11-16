package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestErrorAndPrincipalContext(t *testing.T) {
	t.Parallel()
	base := errors.New("boom")
	e := &Error{Reason: ReasonExpired, Err: base}
	if got := e.Error(); !strings.Contains(got, "expired") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(e, base) {
		t.Fatal("Unwrap did not expose the underlying error")
	}
	if got := (&Error{Reason: ReasonRevoked}).Error(); got != "revoked" {
		t.Fatalf("Error() without cause = %q", got)
	}

	p := &Principal{Kind: "jwt", ID: "user-1"}
	ctx := WithPrincipal(context.Background(), p)
	got, ok := FromContext(ctx)
	if !ok || got != p {
		t.Fatalf("FromContext = %+v, %v", got, ok)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext found a principal in an empty context")
	}

	if !(*Principal)(nil).IsAnonymous() || (&Principal{Kind: "jwt"}).IsAnonymous() {
		t.Fatal("IsAnonymous is wrong")
	}
	if (*Principal)(nil).LogID() != "" || (*Principal)(nil).RateLimitIdentity("user") != "" {
		t.Fatal("anonymous principals must have no identity")
	}
	if got := (&Principal{Kind: "jwt", ID: "u"}).RateLimitIdentity("user"); got != "u" {
		t.Fatalf("user identity without owner = %q", got)
	}
	if got := (&Principal{Kind: "jwt", ID: "u"}).RateLimitIdentity("unknown"); got != "u" {
		t.Fatalf("default identity = %q", got)
	}
	if got := (&Principal{Kind: "api_key", ID: "k", Owner: "o"}).RateLimitIdentity("user"); got != "o" {
		t.Fatalf("owner identity = %q", got)
	}
}

func TestErrorRank(t *testing.T) {
	t.Parallel()
	cases := map[Reason]int{
		ReasonRevoked:       5,
		ReasonExpired:       4,
		ReasonNotYetValid:   3,
		ReasonWrongAudience: 2,
		ReasonWrongIssuer:   2,
		ReasonInvalid:       1,
		ReasonMissing:       0,
		ReasonForbidden:     0,
	}
	for reason, want := range cases {
		if got := errorRank(&Error{Reason: reason}); got != want {
			t.Errorf("errorRank(%s) = %d, want %d", reason, got, want)
		}
	}
	if got := errorRank(errors.New("plain")); got != 0 {
		t.Fatalf("errorRank(plain) = %d", got)
	}
	if got := errorRank(nil); got != 0 {
		t.Fatalf("errorRank(nil) = %d", got)
	}
}

func TestCredentialExtraction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		headers   map[string][]string
		present   bool
		ambiguous bool
		bearer    string
		apiKey    string
	}{
		{name: "none"},
		{name: "bearer", headers: map[string][]string{"Authorization": {"Bearer abc"}}, present: true, bearer: "abc"},
		{name: "lowercase scheme", headers: map[string][]string{"Authorization": {"bearer abc"}}, present: true, bearer: "abc"},
		{name: "apikey scheme", headers: map[string][]string{"Authorization": {"ApiKey gw_a_b"}}, present: true, apiKey: "gw_a_b"},
		{name: "same key twice", headers: map[string][]string{
			"Authorization": {"ApiKey gw_a_b"},
			"X-Api-Key":     {"gw_a_b"},
		}, present: true, apiKey: "gw_a_b"},
		{name: "bearer key and header key", headers: map[string][]string{
			"Authorization": {"Bearer gw_a_b"},
			"X-Api-Key":     {"gw_a_b"},
		}, present: true, apiKey: "gw_a_b"},
		{name: "different bearer key", headers: map[string][]string{
			"Authorization": {"Bearer gw_a_b"},
			"X-Api-Key":     {"gw_a_c"},
		}, present: true, ambiguous: true, bearer: "gw_a_b", apiKey: "gw_a_c"},
		{name: "no space", headers: map[string][]string{"Authorization": {"Bearer"}}, present: true, ambiguous: true},
		{name: "unknown scheme", headers: map[string][]string{"Authorization": {"Basic abc"}}, present: true, ambiguous: true},
		{name: "multiple headers", headers: map[string][]string{"Authorization": {"Bearer a", "Bearer b"}}, present: true, ambiguous: true},
		{name: "blank values", headers: map[string][]string{"Authorization": {"   "}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			for k, values := range tc.headers {
				for _, v := range values {
					r.Header.Add(k, v)
				}
			}
			got := extractCredentials(r)
			if got.present != tc.present || got.ambiguous != tc.ambiguous ||
				got.bearer != tc.bearer || got.apiKey != tc.apiKey {
				t.Fatalf("extractCredentials = %+v, want present=%v ambiguous=%v bearer=%q key=%q",
					got, tc.present, tc.ambiguous, tc.bearer, tc.apiKey)
			}
		})
	}
}

func TestAllowedMethodsAndNilVerifiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		methods []string
		jwt     bool
		key     bool
	}{
		{nil, true, true},
		{[]string{"jwt"}, true, false},
		{[]string{"api_key"}, false, true},
		{[]string{"bogus"}, false, false},
	}
	for _, tc := range cases {
		jwt, key := allowedMethods(tc.methods)
		if jwt != tc.jwt || key != tc.key {
			t.Errorf("allowedMethods(%v) = %v, %v", tc.methods, jwt, key)
		}
	}

	a := &Authenticator{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer x")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"jwt"}}); reason(err) != ReasonInvalid ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured JWT error = %v", err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "gw_x_y")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}}); reason(err) != ReasonInvalid ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured API-key error = %v", err)
	}

	// A gw_-prefixed bearer token is normalized to an API key before policy
	// checks, so a JWT-only route rejects it as a disallowed key type.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer gw_x_y")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"jwt"}}); err == nil ||
		!strings.Contains(err.Error(), "API keys are not allowed") {
		t.Fatalf("gw bearer as JWT = %v", err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer gw_x_y")
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true, Methods: []string{"api_key"}}); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("gw bearer as key = %v", err)
	}
}

func TestAuthenticateChoosesHighestRankedError(t *testing.T) {
	t.Setenv("GW_TEST_JWT_SECRET", testSecret)
	a, err := New(config.Auth{
		JWT: config.JWT{
			Algorithms:     []string{"HS256"},
			HS256SecretEnv: "GW_TEST_JWT_SECRET",
			Issuer:         "https://issuer.example",
		},
		APIKeys: []config.APIKey{{ID: "revoked", Hash: keyHash("s3cret"), Revoked: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	r := bearerRequest(signHS(t, claims))
	r.Header.Set("X-API-Key", "gw_revoked_s3cret")
	// Both methods are allowed: the expired JWT ranks below the revoked key.
	if _, err := a.Authenticate(r, config.RouteAuth{Required: true}); reason(err) != ReasonRevoked {
		t.Fatalf("error = %v (%s), want revoked", err, reason(err))
	}
}

func TestHMACSecretLoading(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv("GW_HMAC_ENV", "env-secret")
		secret, err := loadHMACSecret(config.JWT{HS256SecretEnv: "GW_HMAC_ENV"})
		if err != nil || string(secret) != "env-secret" {
			t.Fatalf("secret = %q, err = %v", secret, err)
		}
	})
	t.Run("empty env", func(t *testing.T) {
		t.Setenv("GW_HMAC_EMPTY", "")
		if _, err := loadHMACSecret(config.JWT{HS256SecretEnv: "GW_HMAC_EMPTY"}); err == nil {
			t.Fatal("empty environment secret accepted")
		}
	})
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "jwt.secret")
		if err := os.WriteFile(path, []byte("file-secret\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		secret, err := loadHMACSecret(config.JWT{HS256SecretFile: path})
		if err != nil || string(secret) != "file-secret" {
			t.Fatalf("secret = %q, err = %v", secret, err)
		}

		a, err := New(config.Auth{JWT: config.JWT{
			Algorithms:      []string{"HS256"},
			HS256SecretFile: path,
		}})
		if err != nil {
			t.Fatal(err)
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "file-user",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		signed, err := tok.SignedString([]byte("file-secret"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := a.Authenticate(bearerRequest(signed), config.RouteAuth{Required: true})
		if err != nil || p.ID != "file-user" {
			t.Fatalf("principal = %+v, err = %v", p, err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.secret")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHMACSecret(config.JWT{HS256SecretFile: path}); err == nil {
			t.Fatal("empty secret file accepted")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := loadHMACSecret(config.JWT{HS256SecretFile: filepath.Join(t.TempDir(), "nope")}); err == nil {
			t.Fatal("missing secret file accepted")
		}
	})
	t.Run("no source", func(t *testing.T) {
		if _, err := loadHMACSecret(config.JWT{}); err == nil {
			t.Fatal("missing source accepted")
		}
	})
}

func TestRSAPublicKeyLoading(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := loadRSAPublicKey(""); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := loadRSAPublicKey(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing file accepted")
	}

	noPEM := filepath.Join(dir, "plain.pem")
	if err := os.WriteFile(noPEM, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRSAPublicKey(noPEM); err == nil {
		t.Fatal("non-PEM file accepted")
	}

	malformed := filepath.Join(dir, "malformed.pem")
	writePEMFile(t, malformed, "PUBLIC KEY", []byte("garbage"))
	if _, err := loadRSAPublicKey(malformed); err == nil {
		t.Fatal("malformed PKIX key accepted")
	}

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKIXPublicKey(edPub)
	if err != nil {
		t.Fatal(err)
	}
	notRSA := filepath.Join(dir, "ed25519.pem")
	writePEMFile(t, notRSA, "PUBLIC KEY", edDER)
	if _, err := loadRSAPublicKey(notRSA); err == nil || !strings.Contains(err.Error(), "want RSA") {
		t.Fatalf("non-RSA key error = %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := filepath.Join(dir, "pkcs1.pem")
	writePEMFile(t, pkcs1, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&key.PublicKey))
	if got, err := loadRSAPublicKey(pkcs1); err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("PKCS#1 key = %v, %v", got, err)
	}

	pkixPath := filepath.Join(dir, "pkix.pem")
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEMFile(t, pkixPath, "PUBLIC KEY", der)
	if got, err := loadRSAPublicKey(pkixPath); err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("PKIX key = %v, %v", got, err)
	}

	// Unknown blocks are skipped before the real key.
	mixed := filepath.Join(dir, "mixed.pem")
	f, err := os.Create(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: []byte("ignored")}); err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PUBLIC KEY", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := loadRSAPublicKey(mixed); err != nil {
		t.Fatalf("mixed PEM = %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gateway-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	writePEMFile(t, certPath, "CERTIFICATE", certDER)
	if got, err := loadRSAPublicKey(certPath); err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("certificate key = %v, %v", got, err)
	}

	badCert := filepath.Join(dir, "badcert.pem")
	writePEMFile(t, badCert, "CERTIFICATE", []byte("garbage"))
	if _, err := loadRSAPublicKey(badCert); err == nil {
		t.Fatal("malformed certificate accepted")
	}

	edCertDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, edPriv.Public(), edPriv)
	if err != nil {
		t.Fatal(err)
	}
	edCert := filepath.Join(dir, "edcert.pem")
	writePEMFile(t, edCert, "CERTIFICATE", edCertDER)
	if _, err := loadRSAPublicKey(edCert); err == nil || !strings.Contains(err.Error(), "want RSA") {
		t.Fatalf("non-RSA certificate error = %v", err)
	}
}

func TestJWTVerifyEdgeCases(t *testing.T) {
	t.Setenv("GW_TEST_JWT_SECRET", testSecret)
	authenticator := hsAuthenticator(t)

	t.Run("token too large", func(t *testing.T) {
		_, err := authenticator.Authenticate(bearerRequest(strings.Repeat("a", maxTokenBytes+1)), requiredJWT())
		if reason(err) != ReasonInvalid {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("subject too long", func(t *testing.T) {
		claims := validClaims()
		claims["sub"] = strings.Repeat("s", 257)
		if _, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); reason(err) != ReasonInvalid {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("blank subject", func(t *testing.T) {
		claims := validClaims()
		claims["sub"] = "   "
		if _, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); reason(err) != ReasonInvalid {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("numeric subject falls back to client_id", func(t *testing.T) {
		claims := validClaims()
		claims["sub"] = 123
		claims["client_id"] = "client-9"
		p, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT())
		if err != nil || p.ID != "client-9" {
			t.Fatalf("principal = %+v, err = %v", p, err)
		}
	})
	t.Run("numeric audience", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = 123
		if _, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); reason(err) != ReasonWrongAudience {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing audience", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "aud")
		if _, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); reason(err) != ReasonWrongAudience {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("audience array match", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{"other", "gateway"}
		if _, err := authenticator.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); err != nil {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("no audience configured", func(t *testing.T) {
		a, err := New(config.Auth{JWT: config.JWT{
			Algorithms:     []string{"HS256"},
			HS256SecretEnv: "GW_TEST_JWT_SECRET",
		}})
		if err != nil {
			t.Fatal(err)
		}
		claims := jwt.MapClaims{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()}
		if _, err := a.Authenticate(bearerRequest(signHS(t, claims)), requiredJWT()); err != nil {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestJWTKeyFuncBranches(t *testing.T) {
	t.Parallel()
	rsaKey := &rsa.PublicKey{N: big.NewInt(1), E: 65537}
	v := &jwtVerifier{
		allowed: map[string]struct{}{"HS256": {}, "RS256": {}, "none": {}},
		hmacKey: []byte("secret"),
		rsaKey:  rsaKey,
	}
	claims := jwt.MapClaims{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()}

	withAlg := func(method jwt.SigningMethod, alg string) *jwt.Token {
		tok := jwt.NewWithClaims(method, claims)
		tok.Header["alg"] = alg
		return tok
	}

	if _, err := v.keyFunc(withAlg(jwt.SigningMethodHS256, "HS384")); err == nil {
		t.Fatal("disallowed algorithm accepted")
	}
	if _, err := v.keyFunc(withAlg(jwt.SigningMethodHS384, "HS256")); err != nil {
		t.Fatalf("HMAC family keyFunc rejected: %v", err)
	}
	if _, err := v.keyFunc(withAlg(jwt.SigningMethodHS256, "RS256")); err == nil {
		t.Fatal("HMAC method accepted for RS256")
	}
	if _, err := v.keyFunc(withAlg(jwt.SigningMethodHS256, "none")); err == nil {
		t.Fatal("unsupported algorithm accepted")
	}
	if _, err := v.keyFunc(withAlg(jwt.SigningMethodHS256, "HS256")); err != nil {
		t.Fatalf("HS256 keyFunc = %v", err)
	}
	if _, err := v.keyFunc(withAlg(jwt.SigningMethodRS256, "RS256")); err != nil {
		t.Fatalf("RS256 keyFunc = %v", err)
	}

	missing := &jwtVerifier{allowed: map[string]struct{}{"HS256": {}, "RS256": {}}}
	if _, err := missing.keyFunc(withAlg(jwt.SigningMethodHS256, "HS256")); err == nil {
		t.Fatal("missing HMAC key accepted")
	}
	if _, err := missing.keyFunc(withAlg(jwt.SigningMethodRS256, "RS256")); err == nil {
		t.Fatal("missing RSA key accepted")
	}
}

func TestJWTConstructionErrors(t *testing.T) {
	t.Parallel()
	if _, err := newJWTVerifier(config.JWT{Algorithms: []string{"ES256"}}); err == nil {
		t.Fatal("unsupported algorithm accepted")
	}
	if _, err := newJWTVerifier(config.JWT{Algorithms: []string{"HS256"}}); err == nil {
		t.Fatal("HS256 without a secret accepted")
	}
	if _, err := newJWTVerifier(config.JWT{Algorithms: []string{"RS256"}}); err == nil {
		t.Fatal("RS256 without a key accepted")
	}
}

func TestJWTClassifyAndClaimHelpers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want Reason
	}{
		{fmt.Errorf("wrapped: %w", jwt.ErrTokenExpired), ReasonExpired},
		{jwt.ErrTokenNotValidYet, ReasonNotYetValid},
		{jwt.ErrTokenInvalidAudience, ReasonWrongAudience},
		{jwt.ErrTokenInvalidIssuer, ReasonWrongIssuer},
		{jwt.ErrTokenMalformed, ReasonInvalid},
		{errors.New("other"), ReasonInvalid},
	}
	for _, tc := range cases {
		if got := classifyJWTError(tc.err); got != tc.want {
			t.Errorf("classifyJWTError(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}

	claims := jwt.MapClaims{
		"str":   "a b a",
		"slice": []string{"x", ""},
		"mixed": []any{"y", 7, "", "z"},
		"other": 42,
	}
	if got := strings.Join(claimList(claims, "str"), ","); got != "a,b,a" {
		t.Errorf("string claim = %q", got)
	}
	if got := strings.Join(claimList(claims, "slice"), ","); got != "x," {
		t.Errorf("slice claim = %q", got)
	}
	if got := strings.Join(claimList(claims, "mixed"), ","); got != "y,z" {
		t.Errorf("mixed claim = %q", got)
	}
	if got := claimList(claims, "other"); got != nil {
		t.Errorf("numeric claim = %v", got)
	}
	if got := claimList(claims, "missing"); got != nil {
		t.Errorf("missing claim = %v", got)
	}

	if got := dedupe([]string{"a", "", "a", "b"}); strings.Join(got, ",") != "a,b" {
		t.Errorf("dedupe = %v", got)
	}
	if got := dedupe(nil); got != nil {
		t.Errorf("dedupe(nil) = %v", got)
	}
}

func TestAPIKeyVerifierEdgeCases(t *testing.T) {
	t.Parallel()
	v := &apiKeyVerifier{
		keys: map[string]config.APIKey{"bad": {ID: "bad", Hash: "not-hex"}},
		now:  time.Now,
	}
	if _, err := v.verify("gw_bad_secret"); reason(err) != ReasonInvalid {
		t.Fatalf("invalid stored record error = %v", err)
	}
	if _, err := v.verify(strings.Repeat("g", maxTokenBytes+1)); reason(err) != ReasonInvalid {
		t.Fatalf("oversized key error = %v", err)
	}
	if _, err := v.verify("gw_a_b_c"); reason(err) != ReasonInvalid {
		// SplitN(3) turns this into id=a, secret=b_c, which is a well-formed
		// but unknown key.
		t.Fatalf("unknown formatted key error = %v", err)
	}
	if _, err := v.verify("gw__"); reason(err) != ReasonInvalid {
		t.Fatalf("empty key error = %v", err)
	}
}
