package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
server:
  addr: ":8080"
admin:
  enabled: false
metrics:
  enabled: false
upstreams:
  - name: users
    targets:
      - url: http://127.0.0.1:9001
routes:
  - name: users
    match:
      path_prefix: /api/users
    upstream: users
`

func TestParseMinimalDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":8080" || cfg.Server.ReadHeaderTimeout.Duration != 10*time.Second {
		t.Fatalf("server defaults: %+v", cfg.Server)
	}
	if cfg.Admin.IsEnabled() || cfg.Metrics.IsEnabled() {
		t.Fatal("admin/metrics should be disabled")
	}
	if !cfg.Redis.FailOpenEnabled() {
		t.Fatal("redis fail_open should default true")
	}
	u := cfg.Upstreams[0]
	if u.Balancer != "round_robin" || u.Targets[0].Weight != 1 {
		t.Fatalf("upstream defaults: %+v", u)
	}
	if !u.Breaker.IsEnabled() || u.Breaker.FailureThreshold != 5 || u.Breaker.Window.Duration != 10*time.Second {
		t.Fatalf("breaker defaults: %+v", u.Breaker)
	}
	if !u.Retry.IsEnabled() || u.Retry.MaxAttempts != 2 || len(u.Retry.On) != 4 {
		t.Fatalf("retry defaults: %+v", u.Retry)
	}
	if u.Transport.MaxIdleConnsPerHost != 256 || u.HealthCheck.Path != "/healthz" {
		t.Fatalf("transport/health defaults: %+v %+v", u.Transport, u.HealthCheck)
	}
	r := cfg.Routes[0]
	if r.Timeout.Duration != 30*time.Second || r.MaxBodyBytes != 1<<20 {
		t.Fatalf("route defaults: %+v", r)
	}
	if r.Auth.Match != "any" {
		t.Fatalf("auth match = %q", r.Auth.Match)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "\nbogus_field: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field bogus_field not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestParseRejectsExtraDocuments(t *testing.T) {
	_, err := Parse([]byte(minimalYAML + "\n---\nserver: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected multi-document error, got %v", err)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, data := range []string{"", "   ", "# comment only\n"} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("expected error for %q", data)
		}
	}
}

func TestParseDurationForms(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(minimalYAML,
		"addr: \":8080\"", "addr: \":8080\"\n  read_header_timeout: 3\n  idle_timeout: 2s", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ReadHeaderTimeout.Duration != 3*time.Second {
		t.Fatalf("integer duration = %v", cfg.Server.ReadHeaderTimeout.Duration)
	}
	if cfg.Server.IdleTimeout.Duration != 2*time.Second {
		t.Fatalf("string duration = %v", cfg.Server.IdleTimeout.Duration)
	}
	_, err = Parse([]byte(strings.Replace(minimalYAML,
		"addr: \":8080\"", "addr: \":8080\"\n  read_header_timeout: nope", 1)))
	if err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func validConfig() *Config {
	disabled := false
	return &Config{
		Admin:   Admin{Enabled: &disabled},
		Metrics: Metrics{Enabled: &disabled},
		Upstreams: []Upstream{{
			Name:    "users",
			Targets: []Target{{URL: "http://127.0.0.1:9001"}},
		}},
		Routes: []Route{{
			Name:     "users",
			Match:    Match{PathPrefix: "/api/users"},
			Upstream: "users",
		}},
	}
}

func TestValidateValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAggregates(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Upstream = "missing"
	cfg.Routes[0].Match.PathPrefix = "no-slash"
	cfg.Upstreams[0].Targets[0].URL = "ftp://x"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error type %T", err)
	}
	if len(ve.Problems) < 3 {
		t.Fatalf("expected aggregated problems, got %v", ve.Problems)
	}
}

func TestValidationRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"duplicate upstream", func(c *Config) { c.Upstreams = append(c.Upstreams, c.Upstreams[0]) }, "duplicate name"},
		{"duplicate route", func(c *Config) { c.Routes = append(c.Routes, c.Routes[0]) }, "duplicate name"},
		{"no targets", func(c *Config) { c.Upstreams[0].Targets = nil }, "at least one target"},
		{"bad scheme", func(c *Config) { c.Upstreams[0].Targets[0].URL = "ftp://x" }, "scheme"},
		{"userinfo", func(c *Config) { c.Upstreams[0].Targets[0].URL = "http://u:p@x" }, "userinfo"},
		{"query target", func(c *Config) { c.Upstreams[0].Targets[0].URL = "http://x/?a=1" }, "query"},
		{"bad balancer", func(c *Config) { c.Upstreams[0].Balancer = "magic" }, "unknown balancer"},
		{"bad breaker ratio", func(c *Config) { c.Upstreams[0].Breaker.FailureRatio = 2 }, "failure_ratio"},
		{"bad retry attempts", func(c *Config) { c.Upstreams[0].Retry.MaxAttempts = 99 }, "max_attempts"},
		{"bad retry reason", func(c *Config) { c.Upstreams[0].Retry.On = []string{"nope"} }, "unknown reason"},
		{"bad health range", func(c *Config) { c.Upstreams[0].HealthCheck.ExpectedStatusMax = 99 }, "status range"},
		{"bad path", func(c *Config) { c.Routes[0].Match.PathPrefix = "/api//users" }, "normalized"},
		{"bad host", func(c *Config) { c.Routes[0].Match.Host = "*.bad*host" }, "wildcard"},
		{"bad method", func(c *Config) { c.Routes[0].Match.Methods = []string{"BAD METHOD"} }, "invalid method"},
		{"strip not prefix", func(c *Config) { c.Routes[0].StripPrefix = "/other" }, "not a prefix"},
		{"strip boundary", func(c *Config) { c.Routes[0].StripPrefix = "/ap" }, "path boundary"},
		{"scopes without auth", func(c *Config) { c.Routes[0].Auth.Scopes = []string{"x"} }, "require auth.required"},
		{"bad auth match", func(c *Config) {
			c.Routes[0].Auth = RouteAuth{Required: true, Methods: []string{"api_key"}, Match: "some"}
			c.Auth.APIKeys = []APIKey{{ID: "k", Hash: strings.Repeat("a", 64)}}
		}, "match must be"},
		{"bad rate key", func(c *Config) { c.Routes[0].RateLimit = &RateLimit{Capacity: 1, RefillPerSec: 1, Key: "bogus"} }, "rate_limit.key"},
		{"missing header", func(c *Config) { c.Routes[0].RateLimit = &RateLimit{Capacity: 1, RefillPerSec: 1, Key: "header"} }, "header is required"},
		{"bad timeout", func(c *Config) { c.Routes[0].Timeout.Duration = -1 }, "timeout"},
		{"bad body", func(c *Config) { c.Routes[0].MaxBodyBytes = -1 }, "max_body_bytes"},
		{"bad log level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"bad trusted proxy", func(c *Config) { c.Server.TrustedProxies = []string{"nope"} }, "trusted_proxies"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateRouteTies(t *testing.T) {
	cfg := validConfig()
	disabled := false
	cfg.Admin.Enabled = &disabled
	cfg.Metrics.Enabled = &disabled
	second := cfg.Routes[0]
	second.Name = "second"
	cfg.Routes = append(cfg.Routes, second)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous match error, got %v", err)
	}

	cfg = validConfig()
	second = cfg.Routes[0]
	second.Name = "post-only"
	second.Match.Methods = []string{"POST"}
	cfg.Routes[0].Match.Methods = []string{"GET"}
	cfg.Routes = append(cfg.Routes, second)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disjoint methods should be valid: %v", err)
	}
}

func TestValidateAuthAvailability(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].Auth = RouteAuth{Required: true, Methods: []string{"jwt"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no configured authentication") {
		t.Fatalf("expected auth availability error, got %v", err)
	}

	cfg = validConfig()
	cfg.Auth.APIKeys = []APIKey{{ID: "k", Hash: strings.Repeat("a", 64)}}
	cfg.Routes[0].Auth = RouteAuth{Required: true, Methods: []string{"jwt", "api_key"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("api_key alone should satisfy route: %v", err)
	}
}

func TestValidateJWT(t *testing.T) {
	tests := []struct {
		name    string
		jwt     JWT
		wantSub string
	}{
		{"hs missing key", JWT{Algorithms: []string{"HS256"}}, "hs256_secret"},
		{"hs ok", JWT{Algorithms: []string{"HS256"}, HS256SecretEnv: "X"}, ""},
		{"both key sources", JWT{Algorithms: []string{"HS256"}, HS256SecretEnv: "X", HS256SecretFile: "f"}, "only one"},
		{"rs missing file", JWT{Algorithms: []string{"RS256"}}, "rs256_public_key_file"},
		{"unsupported", JWT{Algorithms: []string{"HS512"}, HS256SecretEnv: "X"}, "unsupported algorithm"},
		{"leeway", JWT{Leeway: Duration{6 * time.Second}}, "leeway"},
		{"negative leeway", JWT{Leeway: Duration{-time.Second}}, "leeway"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Auth.JWT = tc.jwt
			cfg.Routes[0].Auth = RouteAuth{Required: true, Methods: []string{"jwt"}}
			err := cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateAPIKeyHash(t *testing.T) {
	cfg := validConfig()
	cfg.Auth.APIKeys = []APIKey{{ID: "k", Hash: "bad"}}
	cfg.Routes[0].Auth = RouteAuth{Required: true, Methods: []string{"api_key"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "64-character hex") {
		t.Fatalf("expected hash error, got %v", err)
	}
}

func TestFailOpenPolicy(t *testing.T) {
	cfg := validConfig()
	cfg.Routes[0].RateLimit = &RateLimit{Capacity: 10, RefillPerSec: 5, Key: "ip"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.HasRateLimits() || cfg.HasStrictRateLimits() {
		t.Fatal("default should be rate limited and fail-open")
	}
	f := false
	cfg.Redis.FailOpen = &f
	if !cfg.HasStrictRateLimits() {
		t.Fatal("expected strict policy")
	}
	tr := true
	cfg.Routes[0].RateLimit.FailOpen = &tr
	if cfg.HasStrictRateLimits() {
		t.Fatal("route override should make it fail-open")
	}
}

func TestRedaction(t *testing.T) {
	cfg := validConfig()
	cfg.Auth.APIKeys = []APIKey{{ID: "k", Hash: strings.Repeat("ab", 32), Scopes: []string{"x"}}}
	red := cfg.Redacted()
	if red.Auth.APIKeys[0].Hash != "[REDACTED]" {
		t.Fatalf("hash = %q", red.Auth.APIKeys[0].Hash)
	}
	if cfg.Auth.APIKeys[0].Hash == "[REDACTED]" {
		t.Fatal("redaction mutated the source config")
	}
	out, err := RedactedYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), strings.Repeat("ab", 32)) || !strings.Contains(string(out), "[REDACTED]") {
		t.Fatalf("unexpected redacted YAML:\n%s", out)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(minimalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Name != "users" {
		t.Fatalf("routes = %+v", cfg.Routes)
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected missing file error")
	}
}

func TestDisplayHelpers(t *testing.T) {
	d := Duration{1500 * time.Millisecond}
	if d.String() != "1.5s" {
		t.Fatalf("String = %q", d.String())
	}
	ve := &ValidationError{Problems: []string{"a", "b"}}
	if !strings.Contains(ve.Error(), "a; b") {
		t.Fatalf("Error = %q", ve.Error())
	}
}
