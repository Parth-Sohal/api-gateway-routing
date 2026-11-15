// Command token mints short-lived HS256 JWTs for local development and prints
// SHA-256 hashes for API-key records. It reads signing secrets from a flag,
// an environment variable or a file and never logs them.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

const (
	defaultTTL = time.Hour
	maxTTL     = 24 * time.Hour
	maxList    = 32
	maxItem    = 128
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	secret := fs.String("secret", os.Getenv("GATEWAY_JWT_SECRET"), "HS256 signing secret (defaults to $GATEWAY_JWT_SECRET)")
	secretFile := fs.String("secret-file", "", "file containing the HS256 signing secret")
	hashSecret := fs.String("hash-secret", "", "print the SHA-256 hex of this API-key secret and exit")
	sub := fs.String("sub", "demo-user", "JWT subject")
	scopes := fs.String("scopes", "", "comma/space separated scopes")
	roles := fs.String("roles", "", "comma/space separated roles")
	issuer := fs.String("issuer", "", "optional iss claim")
	audience := fs.String("audience", "", "optional comma separated aud claim")
	ttlFlag := fs.Duration("ttl", defaultTTL, "token lifetime (maximum 24h)")
	alg := fs.String("alg", "HS256", "signing algorithm (HS256 only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "token: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	if *hashSecret != "" {
		sum := sha256.Sum256([]byte(*hashSecret))
		fmt.Fprintln(stdout, hex.EncodeToString(sum[:]))
		return 0
	}
	if !strings.EqualFold(strings.TrimSpace(*alg), "HS256") {
		fmt.Fprintf(stderr, "token: unsupported -alg %q (only HS256 is minted by this tool)\n", *alg)
		return 2
	}
	if *ttlFlag <= 0 || *ttlFlag > maxTTL {
		fmt.Fprintf(stderr, "token: -ttl must be positive and at most %s\n", maxTTL)
		return 2
	}
	subject := strings.TrimSpace(*sub)
	if subject == "" {
		fmt.Fprintln(stderr, "token: -sub must not be empty")
		return 2
	}
	if len(subject) > 256 {
		fmt.Fprintln(stderr, "token: -sub is too long")
		return 2
	}
	key, err := signingKey(*secret, *secretFile)
	if err != nil {
		fmt.Fprintf(stderr, "token: %v\n", err)
		return 2
	}
	scopeList := parseList(*scopes)
	roleList := parseList(*roles)
	audienceList := parseList(*audience)

	now := time.Now()
	claims := jwt.MapClaims{
		"sub": subject,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(*ttlFlag).Unix(),
		"jti": httpx.NewRequestID(),
	}
	if iss := strings.TrimSpace(*issuer); iss != "" {
		claims["iss"] = iss
	}
	if len(audienceList) > 0 {
		claims["aud"] = audienceList
	}
	if len(scopeList) > 0 {
		claims["scope"] = strings.Join(scopeList, " ")
	}
	if len(roleList) > 0 {
		claims["roles"] = roleList
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		fmt.Fprintf(stderr, "token: sign: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, signed)
	return 0
}

func signingKey(secret, file string) ([]byte, error) {
	if secret != "" {
		return []byte(secret), nil
	}
	if strings.TrimSpace(file) == "" {
		return nil, fmt.Errorf("no signing secret: pass -secret, set GATEWAY_JWT_SECRET, or pass -secret-file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	key := strings.TrimRight(string(data), "\r\n")
	if key == "" {
		return nil, fmt.Errorf("secret file %s is empty", file)
	}
	return []byte(key), nil
}

func parseList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, item := range fields {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len(item) > maxItem {
			item = item[:maxItem]
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
		if len(out) == maxList {
			break
		}
	}
	return out
}
