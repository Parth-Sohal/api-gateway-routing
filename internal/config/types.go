// Package config loads, validates and watches the gateway's YAML
// configuration. A Config value is immutable after Parse/LoadFile returns; hot
// reload builds a complete new value and swaps it atomically at a higher layer.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root document.
type Config struct {
	Server    Server     `yaml:"server"`
	Admin     Admin      `yaml:"admin"`
	Metrics   Metrics    `yaml:"metrics"`
	Redis     Redis      `yaml:"redis"`
	Auth      Auth       `yaml:"auth"`
	Log       Log        `yaml:"log"`
	Upstreams []Upstream `yaml:"upstreams"`
	Routes    []Route    `yaml:"routes"`
}

// Server configures the public listener.
type Server struct {
	Addr                  string   `yaml:"addr"`
	ReadHeaderTimeout     Duration `yaml:"read_header_timeout"`
	ReadTimeout           Duration `yaml:"read_timeout"`
	WriteTimeout          Duration `yaml:"write_timeout"`
	IdleTimeout           Duration `yaml:"idle_timeout"`
	MaxHeaderBytes        int      `yaml:"max_header_bytes"`
	ShutdownGrace         Duration `yaml:"shutdown_grace"`
	TrustedProxies        []string `yaml:"trusted_proxies"`
	MaxConcurrentRequests int      `yaml:"max_concurrent_requests"`
}

// Admin configures the private admin listener.
type Admin struct {
	Enabled           *bool    `yaml:"enabled"`
	Addr              string   `yaml:"addr"`
	TokenEnv          string   `yaml:"token_env"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`
}

// Metrics configures the Prometheus scrape listener.
type Metrics struct {
	Enabled *bool  `yaml:"enabled"`
	Addr    string `yaml:"addr"`
	Path    string `yaml:"path"`
}

// Redis configures shared rate-limit state. Redis is only contacted when at
// least one route has a rate_limit block.
type Redis struct {
	Addr         string   `yaml:"addr"`
	PasswordEnv  string   `yaml:"password_env"`
	DB           int      `yaml:"db"`
	PoolSize     int      `yaml:"pool_size"`
	MinIdleConns int      `yaml:"min_idle_conns"`
	DialTimeout  Duration `yaml:"dial_timeout"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	OpTimeout    Duration `yaml:"op_timeout"`
	FailOpen     *bool    `yaml:"fail_open"`
}

// Auth configures JWT and API-key verification.
type Auth struct {
	JWT     JWT      `yaml:"jwt"`
	APIKeys []APIKey `yaml:"api_keys"`
}

// JWT configures accepted algorithms and key material references. Secrets and
// private keys are never stored in YAML.
type JWT struct {
	Algorithms         []string `yaml:"algorithms"`
	HS256SecretEnv     string   `yaml:"hs256_secret_env"`
	HS256SecretFile    string   `yaml:"hs256_secret_file"`
	RS256PublicKeyFile string   `yaml:"rs256_public_key_file"`
	Issuer             string   `yaml:"issuer"`
	Audience           []string `yaml:"audience"`
	Leeway             Duration `yaml:"leeway"`
	ScopesClaim        string   `yaml:"scopes_claim"`
}

// APIKey is a hashed API-key record. Hash is the lowercase hex SHA-256 of the
// secret part of the key.
type APIKey struct {
	ID        string     `yaml:"id"`
	Owner     string     `yaml:"owner"`
	Hash      string     `yaml:"hash"`
	Scopes    []string   `yaml:"scopes"`
	Roles     []string   `yaml:"roles"`
	Revoked   bool       `yaml:"revoked"`
	ExpiresAt *time.Time `yaml:"expires_at"`
}

// Log configures structured logging.
type Log struct {
	Level     string `yaml:"level"`
	Format    string `yaml:"format"`
	AccessLog *bool  `yaml:"access_log"`
}

// Upstream is a named pool of targets with resilience settings.
type Upstream struct {
	Name        string        `yaml:"name"`
	Targets     []Target      `yaml:"targets"`
	Balancer    string        `yaml:"balancer"`
	Transport   Transport     `yaml:"transport"`
	HealthCheck HealthCheck   `yaml:"health_check"`
	Breaker     BreakerConfig `yaml:"breaker"`
	Retry       RetryConfig   `yaml:"retry"`
}

// Target is one backend instance.
type Target struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// Transport tunes the per-upstream http.Transport.
type Transport struct {
	MaxIdleConns          int      `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int      `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int      `yaml:"max_conns_per_host"`
	IdleConnTimeout       Duration `yaml:"idle_conn_timeout"`
	DialTimeout           Duration `yaml:"dial_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
	TLSHandshakeTimeout   Duration `yaml:"tls_handshake_timeout"`
	ExpectContinueTimeout Duration `yaml:"expect_continue_timeout"`
}

// HealthCheck configures active probes and passive thresholds. Passive
// ejection is active even when Enabled is false.
type HealthCheck struct {
	Enabled            bool     `yaml:"enabled"`
	Path               string   `yaml:"path"`
	Interval           Duration `yaml:"interval"`
	Timeout            Duration `yaml:"timeout"`
	HealthyThreshold   int      `yaml:"healthy_threshold"`
	UnhealthyThreshold int      `yaml:"unhealthy_threshold"`
	ExpectedStatusMin  int      `yaml:"expected_status_min"`
	ExpectedStatusMax  int      `yaml:"expected_status_max"`
}

// BreakerConfig configures an upstream circuit breaker.
type BreakerConfig struct {
	Enabled           *bool    `yaml:"enabled"`
	FailureThreshold  int      `yaml:"failure_threshold"`
	FailureRatio      float64  `yaml:"failure_ratio"`
	MinRequests       int      `yaml:"min_requests"`
	Window            Duration `yaml:"window"`
	Buckets           int      `yaml:"buckets"`
	OpenTimeout       Duration `yaml:"open_timeout"`
	HalfOpenMaxProbes int      `yaml:"half_open_max_probes"`
	HalfOpenSuccesses int      `yaml:"half_open_successes"`
	Count500          bool     `yaml:"count_500"`
}

// RetryConfig configures retries for idempotent requests.
type RetryConfig struct {
	Enabled         *bool    `yaml:"enabled"`
	MaxAttempts     int      `yaml:"max_attempts"`
	On              []string `yaml:"on"`
	Methods         []string `yaml:"methods"`
	BaseBackoff     Duration `yaml:"base_backoff"`
	MaxBackoff      Duration `yaml:"max_backoff"`
	BodyBufferBytes int      `yaml:"body_buffer_bytes"`
	BudgetPercent   int      `yaml:"budget_percent"`
}

// Route maps matching requests to an upstream and applies policy.
type Route struct {
	Name         string     `yaml:"name"`
	Match        Match      `yaml:"match"`
	Upstream     string     `yaml:"upstream"`
	StripPrefix  string     `yaml:"strip_prefix"`
	PreserveHost bool       `yaml:"preserve_host"`
	Auth         RouteAuth  `yaml:"auth"`
	RateLimit    *RateLimit `yaml:"rate_limit"`
	Timeout      Duration   `yaml:"timeout"`
	MaxBodyBytes int64      `yaml:"max_body_bytes"`
}

// Match selects requests. Empty Methods means any method.
type Match struct {
	Host       string   `yaml:"host"`
	PathPrefix string   `yaml:"path_prefix"`
	Methods    []string `yaml:"methods"`
}

// RouteAuth describes authentication and authorization requirements.
type RouteAuth struct {
	Required bool     `yaml:"required"`
	Methods  []string `yaml:"methods"`
	Scopes   []string `yaml:"scopes"`
	Roles    []string `yaml:"roles"`
	Match    string   `yaml:"match"` // any | all
}

// RateLimit is a token-bucket rule.
type RateLimit struct {
	Capacity     int64   `yaml:"capacity"`
	RefillPerSec float64 `yaml:"refill_per_sec"`
	Key          string  `yaml:"key"` // ip | user | api_key | header
	Header       string  `yaml:"header"`
	FailOpen     *bool   `yaml:"fail_open"`
}

// Duration is a YAML-friendly time.Duration. Values may be strings like "15s"
// or integer seconds.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if n.Tag == "!!int" {
		var secs int64
		if err := n.Decode(&secs); err != nil {
			return err
		}
		d.Duration = time.Duration(secs) * time.Second
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML renders a duration as a Go duration string.
func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// String implements fmt.Stringer.
func (d Duration) String() string { return d.Duration.String() }

// Helpers for tri-state booleans and normalized settings.

func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Enabled reports whether the admin listener is enabled (default true).
func (a Admin) IsEnabled() bool { return boolValue(a.Enabled, true) }

// Enabled reports whether the metrics listener is enabled (default true).
func (m Metrics) IsEnabled() bool { return boolValue(m.Enabled, true) }

// AccessLogEnabled reports whether access logging is enabled (default true).
func (l Log) AccessLogEnabled() bool { return boolValue(l.AccessLog, true) }

// IsEnabled reports whether the breaker is enabled (default true).
func (b BreakerConfig) IsEnabled() bool { return boolValue(b.Enabled, true) }

// IsEnabled reports whether retries are enabled (default true).
func (r RetryConfig) IsEnabled() bool { return boolValue(r.Enabled, true) }

// FailOpenOr returns the route override or the Redis default.
func (r *RateLimit) FailOpenOr(def bool) bool {
	if r == nil || r.FailOpen == nil {
		return def
	}
	return *r.FailOpen
}

// FailOpenEnabled reports the global Redis failure policy (default true).
func (r Redis) FailOpenEnabled() bool { return boolValue(r.FailOpen, true) }

// UpstreamByName returns the named upstream.
func (c *Config) UpstreamByName(name string) (*Upstream, bool) {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i], true
		}
	}
	return nil, false
}

// HasRateLimits reports whether any route is rate limited.
func (c *Config) HasRateLimits() bool {
	for i := range c.Routes {
		if c.Routes[i].RateLimit != nil {
			return true
		}
	}
	return false
}

// HasStrictRateLimits reports whether any rate-limited route uses
// fail_open: false, in which case Redis is a hard startup/readiness dependency.
func (c *Config) HasStrictRateLimits() bool {
	for i := range c.Routes {
		rl := c.Routes[i].RateLimit
		if rl != nil && !rl.FailOpenOr(c.Redis.FailOpenEnabled()) {
			return true
		}
	}
	return false
}
