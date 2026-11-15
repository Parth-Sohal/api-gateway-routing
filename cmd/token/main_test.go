package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/auth"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func writeSecret(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jwt.secret")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifyWithFile validates a minted token with the production verifier, using
// a mounted secret file so tests stay parallel-safe.
func verifyWithFile(t *testing.T, secretPath string, audience []string, token string) (*auth.Principal, error) {
	t.Helper()
	a, err := auth.New(config.Auth{JWT: config.JWT{
		Algorithms:      []string{"HS256"},
		HS256SecretFile: secretPath,
		Audience:        audience,
	}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return a.Authenticate(req, config.RouteAuth{Required: true, Methods: []string{"jwt"}})
}

func mint(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("run(%v) = %d, stderr = %q", args, code, errOut.String())
	}
	token := strings.TrimSpace(out.String())
	if token == "" {
		t.Fatalf("run(%v) printed no token", args)
	}
	return token
}

func TestMintRoundTrip(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, "super-secret-value\n")
	token := mint(t,
		"-secret-file", path,
		"-sub", "alice",
		"-scopes", "users:read, users:write users:read",
		"-roles", "admin viewer",
		"-issuer", "gateway-tests",
		"-audience", "gateway, internal",
		"-ttl", "90s",
	)

	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims["sub"] != "alice" || claims["iss"] != "gateway-tests" {
		t.Fatalf("claims = %v", claims)
	}
	if got := claims["scope"]; got != "users:read users:write" {
		t.Fatalf("scope claim = %v", got)
	}
	if jti, _ := claims["jti"].(string); len(jti) != 26 {
		t.Fatalf("jti = %q", claims["jti"])
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("exp = %v, %v", exp, err)
	}
	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		t.Fatalf("iat = %v, %v", iat, err)
	}
	if ttl := exp.Sub(iat.Time); ttl != 90*time.Second {
		t.Fatalf("ttl = %v", ttl)
	}
	if ttl := time.Until(exp.Time); ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("token lifetime = %v", ttl)
	}

	principal, err := verifyWithFile(t, path, []string{"gateway"}, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if principal.ID != "alice" || principal.Kind != "jwt" {
		t.Fatalf("principal = %+v", principal)
	}
	if strings.Join(principal.Scopes, ",") != "users:read,users:write" {
		t.Fatalf("scopes = %v", principal.Scopes)
	}
	if strings.Join(principal.Roles, ",") != "admin,viewer" {
		t.Fatalf("roles = %v", principal.Roles)
	}
}

func TestMintMinimumClaims(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, "k")
	token := mint(t, "-secret-file", path, "-sub", "bob")
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		t.Fatalf("parse token: %v", err)
	}
	for _, key := range []string{"scope", "roles", "iss", "aud"} {
		if _, ok := claims[key]; ok {
			t.Fatalf("unexpected %q claim in %v", key, claims)
		}
	}
	if _, err := verifyWithFile(t, path, nil, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestMintWrongSecretRejected(t *testing.T) {
	t.Parallel()
	mintPath := writeSecret(t, "one")
	otherPath := writeSecret(t, "two")
	token := mint(t, "-secret-file", mintPath, "-sub", "alice")
	if _, err := verifyWithFile(t, otherPath, nil, token); err == nil {
		t.Fatal("token signed with another secret was accepted")
	}
}

func TestSecretFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_JWT_SECRET", "env-secret")
	path := writeSecret(t, "env-secret")
	token := mint(t, "-sub", "carol")
	if _, err := verifyWithFile(t, path, nil, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSecretFileCRLFAndExplicitSecretWins(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, "file-secret\r\n")
	fromFile := mint(t, "-secret-file", path, "-sub", "dave")
	if _, err := verifyWithFile(t, writeSecret(t, "file-secret"), nil, fromFile); err != nil {
		t.Fatalf("verify file secret: %v", err)
	}

	explicit := mint(t, "-secret", "explicit", "-secret-file", writeSecret(t, "ignored"), "-sub", "dave")
	if _, err := verifyWithFile(t, writeSecret(t, "explicit"), nil, explicit); err != nil {
		t.Fatalf("verify explicit secret: %v", err)
	}
}

func TestHashSecret(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("demo-secret-123"))
	want := hex.EncodeToString(sum[:])
	if want != "264c5c83f07d30c7ccc1083d7e416705272eafa1fc15167e09bed57b7697faed" {
		t.Fatalf("hash drift: %s", want)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"-hash-secret", "demo-secret-123", "-alg", "rs256"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
}

func TestRunErrors(t *testing.T) {
	t.Setenv("GATEWAY_JWT_SECRET", "")
	longSub := strings.Repeat("s", 300)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown flag", []string{"-nope"}, 2},
		{"extra arg", []string{"-secret", "k", "leftover"}, 2},
		{"no secret", []string{"-sub", "alice"}, 2},
		{"empty secret flag", []string{"-secret", "", "-sub", "alice"}, 2},
		{"missing secret file", []string{"-secret-file", filepath.Join(t.TempDir(), "absent")}, 2},
		{"empty secret file", []string{"-secret-file", writeSecret(t, "")}, 2},
		{"unsupported alg", []string{"-secret", "k", "-alg", "none"}, 2},
		{"zero ttl", []string{"-secret", "k", "-ttl", "0s"}, 2},
		{"negative ttl", []string{"-secret", "k", "-ttl", "-5m"}, 2},
		{"oversized ttl", []string{"-secret", "k", "-ttl", "25h"}, 2},
		{"blank subject", []string{"-secret", "k", "-sub", "   "}, 2},
		{"long subject", []string{"-secret", "k", "-sub", longSub}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.want {
				t.Fatalf("run(%v) = %d, want %d (stderr %q)", tc.args, code, tc.want, errOut.String())
			}
			if errOut.Len() == 0 {
				t.Fatalf("run(%v) wrote no diagnostic", tc.args)
			}
		})
	}
}

func TestParseList(t *testing.T) {
	t.Parallel()
	got := parseList(" a,b  a\tc\nd\n")
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("parseList = %v", got)
	}
	if got := parseList(""); got != nil && len(got) != 0 {
		t.Fatalf("parseList(\"\") = %v", got)
	}
	long := strings.Repeat("x", 200) + ",short"
	list := parseList(long)
	if len(list) != 2 || len(list[0]) != maxItem {
		t.Fatalf("long item not bounded: %d %d", len(list), len(list[0]))
	}
	many := make([]string, 0, maxList+10)
	for i := 0; i < maxList+10; i++ {
		many = append(many, string(rune('a'+i%26))+strings.Repeat("z", i%3))
	}
	if got := parseList(strings.Join(many, ",")); len(got) > maxList {
		t.Fatalf("parseList returned %d items, want <= %d", len(got), maxList)
	}
}
