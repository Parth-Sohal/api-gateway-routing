package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func boolPtr(v bool) *bool { return &v }

func TestTypeHelpers(t *testing.T) {
	t.Parallel()
	if !(Log{}).AccessLogEnabled() {
		t.Fatal("access log should default to enabled")
	}
	if (Log{AccessLog: boolPtr(false)}).AccessLogEnabled() {
		t.Fatal("access log override ignored")
	}

	cfg := validConfig()
	if up, ok := cfg.UpstreamByName("users"); !ok || up == nil || up.Name != "users" {
		t.Fatalf("UpstreamByName = %v, %v", up, ok)
	}
	if up, ok := cfg.UpstreamByName("missing"); ok || up != nil {
		t.Fatalf("missing upstream = %v, %v", up, ok)
	}

	disabled := false
	if (Admin{Enabled: &disabled}).IsEnabled() || (Metrics{Enabled: &disabled}).IsEnabled() {
		t.Fatal("explicit false should disable listeners")
	}
	if (BreakerConfig{Enabled: &disabled}).IsEnabled() || (RetryConfig{Enabled: &disabled}).IsEnabled() {
		t.Fatal("explicit false should disable breaker/retry")
	}
	if (Redis{FailOpen: &disabled}).FailOpenEnabled() {
		t.Fatal("explicit fail_open=false ignored")
	}
	rl := &RateLimit{FailOpen: &disabled}
	if rl.FailOpenOr(true) || (*RateLimit)(nil).FailOpenOr(true) != true {
		t.Fatal("FailOpenOr override/default behavior is wrong")
	}
}

func TestDurationYAMLFormsAndErrors(t *testing.T) {
	t.Parallel()
	var d Duration
	if err := yaml.Unmarshal([]byte("3"), &d); err != nil || d.Duration != 3*time.Second {
		t.Fatalf("integer duration = %v, %v", d.Duration, err)
	}
	if err := yaml.Unmarshal([]byte("''"), &d); err != nil || d.Duration != 0 {
		t.Fatalf("empty duration = %v, %v", d.Duration, err)
	}
	if err := yaml.Unmarshal([]byte("2m30s"), &d); err != nil || d.Duration != 150*time.Second {
		t.Fatalf("string duration = %v, %v", d.Duration, err)
	}
	for _, raw := range []string{"[]", "{a: b}", "true", "9223372036854775808s"} {
		if err := yaml.Unmarshal([]byte(raw), &d); err == nil {
			t.Errorf("duration %q accepted", raw)
		}
	}
	if got := (Duration{Duration: time.Second}).String(); got != "1s" {
		t.Fatalf("Duration.String = %q", got)
	}
}

func TestInternalValidationHelpers(t *testing.T) {
	t.Parallel()
	cidrs := []struct {
		raw string
		ok  bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"[::1]", true},
		{"10.0.0.0/8", true},
		{"fd00::/8", true},
		{"", false},
		{"nope", false},
		{"10.0.0.0/99", false},
	}
	for _, tc := range cidrs {
		if err := validateCIDROrIP(tc.raw); (err == nil) != tc.ok {
			t.Errorf("validateCIDROrIP(%q) = %v", tc.raw, err)
		}
	}

	hosts := []struct {
		host string
		ok   bool
	}{
		{"", true},
		{"example.com", true},
		{"*.example.com", true},
		{"*", false},
		{"a*b", false},
		{"*.", false},
		{"example.com:8080", false},
		{"https://example.com", false},
	}
	for _, tc := range hosts {
		if err := validateHostPattern(tc.host); (err == nil) != tc.ok {
			t.Errorf("validateHostPattern(%q) = %v", tc.host, err)
		}
	}

	methods := map[string]bool{
		"GET": true, "post": true, "M-SEARCH": true, "PATCH": true,
		"": false, "GET POST": false, "GE/T": false,
	}
	for method, want := range methods {
		if got := validMethod(method); got != want {
			t.Errorf("validMethod(%q) = %v", method, got)
		}
	}
}

func TestNormalizationHelpers(t *testing.T) {
	t.Parallel()
	balancers := map[string]string{
		"":                  "round_robin",
		"RR":                "round_robin",
		"weighted":          "weighted_round_robin",
		"WRR":               "weighted_round_robin",
		"least-connections": "least_conn",
		"LEASTCONN":         "least_conn",
		"magic":             "magic",
	}
	for in, want := range balancers {
		if got := normalizeBalancer(in); got != want {
			t.Errorf("normalizeBalancer(%q) = %q, want %q", in, got, want)
		}
	}
	authMethods := map[string]string{
		"APIKEY": "api_key", "api-key": "api_key", "JWT": "jwt", "mtls": "mtls",
	}
	for in, want := range authMethods {
		if got := normalizeAuthMethod(in); got != want {
			t.Errorf("normalizeAuthMethod(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"": "", "/": "/", "/api/": "/api", "api": "api"} {
		if got := trimTrailingSlash(in); got != want {
			t.Errorf("trimTrailingSlash(%q) = %q, want %q", in, got, want)
		}
	}
	if got := canonicalUpperUnique([]string{" get ", "GET", "", "post"}); strings.Join(got, ",") != "GET,POST" {
		t.Fatalf("canonicalUpperUnique = %v", got)
	}
	if got := uniqueStrings([]string{" a ", "a", "", "b"}); strings.Join(got, ",") != "a,b" {
		t.Fatalf("uniqueStrings = %v", got)
	}
}

func TestParseExplicitDefaults(t *testing.T) {
	t.Parallel()
	document := `
server:
  addr: "127.0.0.1:8080"
  read_header_timeout: 1s
  read_timeout: 2s
  write_timeout: 3s
  idle_timeout: 4s
  max_header_bytes: 4096
  shutdown_grace: 5s
  max_concurrent_requests: 10
admin:
  enabled: true
  addr: "127.0.0.1:9090"
  token_env: ADMIN_TOKEN
  read_header_timeout: 5s
  shutdown_grace: 10s
metrics:
  enabled: true
  addr: "127.0.0.1:9100"
  path: /custom
log:
  level: debug
  format: text
  access_log: false
auth:
  api_keys:
    - id: demo
      hash: 264c5c83f07d30c7ccc1083d7e416705272eafa1fc15167e09bed57b7697faed
upstreams:
  - name: users
    balancer: wrr
    targets:
      - url: http://127.0.0.1:9001
    health_check:
      enabled: true
    breaker:
      enabled: false
    retry:
      enabled: false
routes:
  - name: users
    match:
      host: API.EXAMPLE.COM
      path_prefix: /api/
      methods: ["*"]
    strip_prefix: /api/
    upstream: users
    auth:
      required: true
      methods: [APIKEY]
`
	cfg, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Server.WriteTimeout.Duration != 3*time.Second || cfg.Server.MaxConcurrentRequests != 10 {
		t.Fatalf("server = %+v", cfg.Server)
	}
	if !cfg.Admin.IsEnabled() || cfg.Admin.TokenEnv != "ADMIN_TOKEN" || !cfg.Metrics.IsEnabled() || cfg.Metrics.Path != "/custom" {
		t.Fatalf("admin/metrics = %+v %+v", cfg.Admin, cfg.Metrics)
	}
	if cfg.Log.AccessLogEnabled() || cfg.Log.Format != "text" {
		t.Fatalf("log = %+v", cfg.Log)
	}
	up := cfg.Upstreams[0]
	if up.Balancer != "weighted_round_robin" || up.Breaker.IsEnabled() || up.Retry.IsEnabled() {
		t.Fatalf("upstream = %+v", up)
	}
	route := cfg.Routes[0]
	if route.Match.Host != "api.example.com" || route.Match.PathPrefix != "/api" ||
		len(route.Match.Methods) != 0 || route.StripPrefix != "/api" {
		t.Fatalf("route match = %+v", route.Match)
	}
	if got := strings.Join(route.Auth.Methods, ","); got != "api_key" {
		t.Fatalf("auth methods = %q", got)
	}
}

func TestValidateOperationalBounds(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.Server.Addr = " "
	cfg.Server.ReadTimeout = Duration{Duration: -time.Second}
	cfg.Server.MaxHeaderBytes = 512
	cfg.Server.ShutdownGrace = Duration{Duration: -time.Second}
	cfg.Server.MaxConcurrentRequests = -1
	cfg.Server.TrustedProxies = []string{"bad-cidr"}
	cfg.Admin.Enabled = boolPtr(true)
	cfg.Admin.Addr = " "
	cfg.Admin.TokenEnv = " "
	cfg.Metrics.Enabled = boolPtr(true)
	cfg.Metrics.Addr = " "
	cfg.Metrics.Path = "metrics"
	cfg.Redis.Addr = " "
	cfg.Redis.PoolSize = 2
	cfg.Redis.MinIdleConns = 3
	cfg.Redis.OpTimeout = Duration{Duration: time.Nanosecond}
	cfg.Redis.DB = -1

	up := &cfg.Upstreams[0]
	up.Balancer = "random"
	up.Transport.MaxIdleConns = -1
	up.Transport.DialTimeout = Duration{Duration: -time.Second}
	up.HealthCheck.Path = "healthz"
	up.HealthCheck.Interval = Duration{Duration: -time.Second}
	up.HealthCheck.ExpectedStatusMin = 99
	up.HealthCheck.ExpectedStatusMax = 600
	up.Breaker.FailureRatio = 2
	up.Breaker.MinRequests = -1
	up.Breaker.Buckets = 101
	up.Retry.MaxAttempts = 50
	up.Retry.BaseBackoff = Duration{Duration: 5 * time.Second}
	up.Retry.MaxBackoff = Duration{Duration: time.Second}
	up.Retry.BodyBufferBytes = -1
	up.Retry.BudgetPercent = 200
	up.Retry.On = []string{"teapot"}
	up.Retry.Methods = []string{"BAD METHOD"}

	route := &cfg.Routes[0]
	route.Match.PathPrefix = "api"
	route.Match.Host = "bad host:8080"
	route.Match.Methods = []string{"BAD METHOD"}
	route.StripPrefix = "/other"
	route.Timeout = Duration{Duration: -time.Second}
	route.MaxBodyBytes = -1
	route.Auth = RouteAuth{Required: true, Methods: []string{"bogus"}, Match: "sometimes", Scopes: []string{"users:read"}}
	route.RateLimit = &RateLimit{Capacity: 0, RefillPerSec: 0, Key: "header"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("invalid operational config accepted")
	}
	for _, want := range []string{
		"server.addr is required",
		"server timeouts must not be negative",
		"server.max_header_bytes",
		"server.max_concurrent_requests",
		"trusted_proxies",
		"admin.addr is required",
		"admin.token_env is required",
		"metrics.addr is required",
		"metrics.path must start with /",
		"redis.addr is required",
		"redis.min_idle_conns",
		"redis.op_timeout",
		"redis.db",
		"unknown balancer",
		"connection counts must not be negative",
		"invalid timeout values",
		"health_check.path",
		"health_check: interval and timeout",
		"invalid expected status range",
		"failure_ratio",
		"min_requests",
		"buckets",
		"max_attempts",
		"backoff values are invalid",
		"body_buffer_bytes",
		"budget_percent",
		"unknown reason",
		"invalid method",
		"path_prefix must start with /",
		"match.host",
		"strip_prefix",
		"timeout must be positive",
		"max_body_bytes",
		"unknown method",
		"auth.match",
		"capacity",
		"refill_per_sec",
		"rate_limit.header is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error missing %q:\n%v", want, err)
		}
	}
}

func TestValidateTargetURLs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		targets []Target
		want    string
	}{
		{"bad scheme", []Target{{URL: "ftp://example.com"}}, "scheme must be http or https"},
		{"no host", []Target{{URL: "http:///path"}}, "host is required"},
		{"userinfo", []Target{{URL: "http://user:pass@example.com"}}, "userinfo is not allowed"},
		{"query", []Target{{URL: "http://example.com/?a=1"}}, "must not contain a query or fragment"},
		{"fragment", []Target{{URL: "http://example.com/#f"}}, "must not contain a query or fragment"},
		{"negative weight", []Target{{URL: "http://example.com", Weight: -1}}, "weight must not be negative"},
		{"duplicate", []Target{{URL: "http://example.com"}, {URL: "http://example.com"}}, "duplicate target"},
		{"malformed", []Target{{URL: "http://[::1"}}, "invalid URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Upstreams[0].Targets = tc.targets
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateLogAndAuthGaps(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.Log.Level = "loud"
	cfg.Log.Format = "xml"
	cfg.Auth.JWT = JWT{
		Algorithms: []string{"RS256", "HS512"},
		Leeway:     Duration{Duration: 10 * time.Second},
		Audience:   []string{" ", "api"},
	}
	cfg.Auth.APIKeys = []APIKey{
		{ID: "bad_id", Hash: "nope"},
		{ID: "bad_id", Hash: strings.Repeat("0", 64)},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("invalid log/auth config accepted")
	}
	for _, want := range []string{
		"log.level", "log.format", "rs256_public_key_file", "unsupported algorithm",
		"leeway", "audience contains an empty value", "duplicate id", "underscores or spaces",
		"hash must be a 64-character hex",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}
