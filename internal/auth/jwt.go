package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

const maxTokenBytes = 8192

type jwtVerifier struct {
	parser      *jwt.Parser
	hmacKey     []byte
	rsaKey      *rsa.PublicKey
	allowed     map[string]struct{}
	audience    []string
	scopesClaim string
}

func newJWTVerifier(cfg config.JWT) (*jwtVerifier, error) {
	if len(cfg.Algorithms) == 0 {
		return nil, nil
	}
	v := &jwtVerifier{
		allowed:     make(map[string]struct{}, len(cfg.Algorithms)),
		audience:    cfg.Audience,
		scopesClaim: cfg.ScopesClaim,
	}
	if v.scopesClaim == "" {
		v.scopesClaim = "scope"
	}
	for _, alg := range cfg.Algorithms {
		switch alg {
		case "HS256", "RS256":
			v.allowed[alg] = struct{}{}
		default:
			return nil, fmt.Errorf("unsupported JWT algorithm %q", alg)
		}
	}

	if _, ok := v.allowed["HS256"]; ok {
		secret, err := loadHMACSecret(cfg)
		if err != nil {
			return nil, err
		}
		v.hmacKey = secret
	}
	if _, ok := v.allowed["RS256"]; ok {
		key, err := loadRSAPublicKey(cfg.RS256PublicKeyFile)
		if err != nil {
			return nil, err
		}
		v.rsaKey = key
	}

	opts := []jwt.ParserOption{
		jwt.WithValidMethods(cfg.Algorithms),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway.Duration),
	}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	v.parser = jwt.NewParser(opts...)
	return v, nil
}

func loadHMACSecret(cfg config.JWT) ([]byte, error) {
	if cfg.HS256SecretEnv != "" {
		secret := os.Getenv(cfg.HS256SecretEnv)
		if secret == "" {
			return nil, fmt.Errorf("JWT secret environment variable %s is empty or unset", cfg.HS256SecretEnv)
		}
		return []byte(secret), nil
	}
	if cfg.HS256SecretFile != "" {
		data, err := os.ReadFile(cfg.HS256SecretFile)
		if err != nil {
			return nil, fmt.Errorf("read JWT secret file: %w", err)
		}
		secret := strings.TrimRight(string(data), "\r\n")
		if secret == "" {
			return nil, errors.New("JWT secret file is empty")
		}
		return []byte(secret), nil
	}
	return nil, errors.New("HS256 is enabled but no secret is configured")
}

func loadRSAPublicKey(path string) (*rsa.PublicKey, error) {
	if path == "" {
		return nil, errors.New("RS256 is enabled but rs256_public_key_file is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read JWT public key: %w", err)
	}
	rest := data
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		switch block.Type {
		case "PUBLIC KEY":
			pub, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse public key: %w", err)
			}
			rsaPub, ok := pub.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("public key is %T, want RSA", pub)
			}
			return rsaPub, nil
		case "RSA PUBLIC KEY":
			rsaPub, err := x509.ParsePKCS1PublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse RSA public key: %w", err)
			}
			return rsaPub, nil
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("certificate public key is %T, want RSA", cert.PublicKey)
			}
			return rsaPub, nil
		}
	}
	return nil, errors.New("no RSA public key or certificate found in PEM file")
}

func (v *jwtVerifier) verify(tokenString string) (*Principal, error) {
	if len(tokenString) > maxTokenBytes {
		return nil, authError(ReasonInvalid, "token too large")
	}
	claims := jwt.MapClaims{}
	token, err := v.parser.ParseWithClaims(tokenString, claims, v.keyFunc)
	if err != nil {
		return nil, &Error{Reason: classifyJWTError(err), Err: err}
	}
	if !token.Valid {
		return nil, authError(ReasonInvalid, "token is not valid")
	}

	if len(v.audience) > 0 {
		got, err := claims.GetAudience()
		if err != nil || !audienceMatches(got, v.audience) {
			return nil, authError(ReasonWrongAudience, "audience does not match")
		}
	}

	sub, err := claims.GetSubject()
	if err != nil || strings.TrimSpace(sub) == "" {
		// Some OAuth2 access tokens use client_id instead of sub.
		sub = stringClaim(claims, "client_id")
	}
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return nil, authError(ReasonInvalid, "token has no subject")
	}
	if len(sub) > 256 {
		return nil, authError(ReasonInvalid, "subject is too long")
	}

	scopes := make([]string, 0, 4)
	for _, name := range scopeClaimNames(v.scopesClaim) {
		scopes = append(scopes, claimList(claims, name)...)
	}
	roles := append(claimList(claims, "roles"), claimList(claims, "role")...)

	return &Principal{
		ID:     sub,
		Kind:   "jwt",
		Scopes: dedupe(scopes),
		Roles:  dedupe(roles),
	}, nil
}

func (v *jwtVerifier) keyFunc(token *jwt.Token) (any, error) {
	alg, _ := token.Header["alg"].(string)
	if _, ok := v.allowed[alg]; !ok {
		return nil, fmt.Errorf("algorithm %q is not allowed", alg)
	}
	switch alg {
	case "HS256":
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %T for HS256", token.Method)
		}
		if len(v.hmacKey) == 0 {
			return nil, errors.New("HS256 key is not configured")
		}
		return v.hmacKey, nil
	case "RS256":
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %T for RS256", token.Method)
		}
		if v.rsaKey == nil {
			return nil, errors.New("RS256 key is not configured")
		}
		return v.rsaKey, nil
	default:
		return nil, fmt.Errorf("algorithm %q is not supported", alg)
	}
}

func classifyJWTError(err error) Reason {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return ReasonExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ReasonNotYetValid
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return ReasonWrongAudience
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return ReasonWrongIssuer
	default:
		return ReasonInvalid
	}
}

func audienceMatches(got []string, want []string) bool {
	set := make(map[string]struct{}, len(got))
	for _, v := range got {
		set[v] = struct{}{}
	}
	for _, v := range want {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

func scopeClaimNames(configured string) []string {
	if configured == "" {
		configured = "scope"
	}
	out := []string{configured}
	for _, name := range []string{"scope", "scopes"} {
		if name != configured {
			out = append(out, name)
		}
	}
	return out
}

func claimList(claims jwt.MapClaims, name string) []string {
	raw, ok := claims[name]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return strings.Fields(v)
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func stringClaim(claims jwt.MapClaims, name string) string {
	if s, ok := claims[name].(string); ok {
		return s
	}
	return ""
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
