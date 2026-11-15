package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// ValidationError aggregates every problem found in a configuration so
// operators can fix them in one pass.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid config: " + e.Problems[0]
	}
	return "invalid config: " + strings.Join(e.Problems, "; ")
}

func (e *ValidationError) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

// Validate normalizes defaults and checks every cross-field invariant. It
// returns a *ValidationError listing all problems.
func (c *Config) Validate() error {
	c.applyDefaults()
	v := &ValidationError{}

	c.validateServer(v)
	c.validateAdminMetrics(v)
	c.validateRedis(v)
	c.validateAuth(v)
	c.validateLog(v)

	upstreams := make(map[string]struct{}, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if strings.TrimSpace(u.Name) == "" {
			v.add("upstreams[%d]: name is required", i)
		} else if _, ok := upstreams[u.Name]; ok {
			v.add("upstreams[%d]: duplicate name %q", i, u.Name)
		} else {
			upstreams[u.Name] = struct{}{}
		}
		c.validateUpstream(v, i, u)
	}

	routes := make(map[string]struct{}, len(c.Routes))
	for i := range c.Routes {
		r := &c.Routes[i]
		if strings.TrimSpace(r.Name) == "" {
			v.add("routes[%d]: name is required", i)
		} else if _, ok := routes[r.Name]; ok {
			v.add("routes[%d]: duplicate name %q", i, r.Name)
		} else {
			routes[r.Name] = struct{}{}
		}
		c.validateRoute(v, i, r, upstreams)
	}
	if len(c.Routes) == 0 {
		v.add("at least one route is required")
	}
	if len(c.Upstreams) == 0 {
		v.add("at least one upstream is required")
	}

	c.validateRouteTies(v)
	c.validateAuthAvailability(v)

	if len(v.Problems) > 0 {
		return v
	}
	return nil
}

func (c *Config) validateServer(v *ValidationError) {
	s := c.Server
	if strings.TrimSpace(s.Addr) == "" {
		v.add("server.addr is required")
	}
	if s.ReadHeaderTimeout.Duration < 0 || s.ReadTimeout.Duration < 0 ||
		s.WriteTimeout.Duration < 0 || s.IdleTimeout.Duration < 0 {
		v.add("server timeouts must not be negative")
	}
	if s.MaxHeaderBytes < 1024 {
		v.add("server.max_header_bytes must be at least 1024")
	}
	if s.ShutdownGrace.Duration < 0 {
		v.add("server.shutdown_grace must not be negative")
	}
	if s.MaxConcurrentRequests < 0 {
		v.add("server.max_concurrent_requests must not be negative")
	}
	for _, raw := range s.TrustedProxies {
		if err := validateCIDROrIP(raw); err != nil {
			v.add("server.trusted_proxies: %v", err)
		}
	}
}

func (c *Config) validateAdminMetrics(v *ValidationError) {
	if c.Admin.IsEnabled() {
		if strings.TrimSpace(c.Admin.Addr) == "" {
			v.add("admin.addr is required when admin is enabled")
		}
		if strings.TrimSpace(c.Admin.TokenEnv) == "" {
			v.add("admin.token_env is required when admin is enabled")
		}
	}
	if c.Metrics.IsEnabled() {
		if strings.TrimSpace(c.Metrics.Addr) == "" {
			v.add("metrics.addr is required when metrics is enabled")
		}
		if !strings.HasPrefix(c.Metrics.Path, "/") {
			v.add("metrics.path must start with /")
		}
	}
}

func (c *Config) validateRedis(v *ValidationError) {
	if !c.HasRateLimits() {
		return
	}
	if strings.TrimSpace(c.Redis.Addr) == "" {
		v.add("redis.addr is required when rate limits are configured")
	}
	if c.Redis.PoolSize < 1 {
		v.add("redis.pool_size must be positive")
	}
	if c.Redis.MinIdleConns < 0 || c.Redis.MinIdleConns > c.Redis.PoolSize {
		v.add("redis.min_idle_conns must be between 0 and pool_size")
	}
	if c.Redis.OpTimeout.Duration < time.Millisecond {
		v.add("redis.op_timeout must be at least 1ms")
	}
	if c.Redis.DB < 0 {
		v.add("redis.db must not be negative")
	}
}

func (c *Config) validateAuth(v *ValidationError) {
	j := c.Auth.JWT
	if j.Leeway.Duration < 0 {
		v.add("auth.jwt.leeway must not be negative")
	} else if j.Leeway.Duration > 5*time.Second {
		v.add("auth.jwt.leeway must be at most 5s")
	}
	for _, alg := range j.Algorithms {
		switch alg {
		case "HS256":
			if j.HS256SecretEnv == "" && j.HS256SecretFile == "" {
				v.add("auth.jwt: HS256 requires hs256_secret_env or hs256_secret_file")
			}
			if j.HS256SecretEnv != "" && j.HS256SecretFile != "" {
				v.add("auth.jwt: set only one of hs256_secret_env and hs256_secret_file")
			}
		case "RS256":
			if j.RS256PublicKeyFile == "" {
				v.add("auth.jwt: RS256 requires rs256_public_key_file")
			}
		default:
			v.add("auth.jwt.algorithms: unsupported algorithm %q (allowed: HS256, RS256)", alg)
		}
	}
	for _, aud := range j.Audience {
		if strings.TrimSpace(aud) == "" {
			v.add("auth.jwt.audience contains an empty value")
		}
	}

	ids := make(map[string]struct{}, len(c.Auth.APIKeys))
	for i, k := range c.Auth.APIKeys {
		if strings.TrimSpace(k.ID) == "" {
			v.add("auth.api_keys[%d]: id is required", i)
		} else if _, ok := ids[k.ID]; ok {
			v.add("auth.api_keys[%d]: duplicate id %q", i, k.ID)
		} else {
			ids[k.ID] = struct{}{}
		}
		raw, err := hex.DecodeString(k.Hash)
		if err != nil || len(raw) != 32 {
			v.add("auth.api_keys[%d] (%s): hash must be a 64-character hex SHA-256", i, k.ID)
		}
		if strings.ContainsAny(k.ID, "_ ") {
			v.add("auth.api_keys[%d]: id must not contain underscores or spaces (key format is gw_<id>_<secret>)", i)
		}
	}
}

func (c *Config) validateLog(v *ValidationError) {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		v.add("log.level must be one of debug, info, warn, error")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		v.add("log.format must be json or text")
	}
}

func (c *Config) validateUpstream(v *ValidationError, i int, u *Upstream) {
	prefix := fmt.Sprintf("upstreams[%d] (%s)", i, u.Name)
	switch u.Balancer {
	case "round_robin", "weighted_round_robin", "least_conn":
	default:
		v.add("%s: unknown balancer %q", prefix, u.Balancer)
	}
	if len(u.Targets) == 0 {
		v.add("%s: at least one target is required", prefix)
	}
	seen := make(map[string]struct{}, len(u.Targets))
	for ti, t := range u.Targets {
		parsed, err := url.Parse(strings.TrimSpace(t.URL))
		if err != nil {
			v.add("%s.targets[%d]: invalid URL: %v", prefix, ti, err)
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			v.add("%s.targets[%d]: scheme must be http or https", prefix, ti)
		}
		if parsed.Host == "" {
			v.add("%s.targets[%d]: host is required", prefix, ti)
		}
		if parsed.User != nil {
			v.add("%s.targets[%d]: userinfo is not allowed in target URLs", prefix, ti)
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			v.add("%s.targets[%d]: target URL must not contain a query or fragment", prefix, ti)
		}
		if t.Weight < 0 {
			v.add("%s.targets[%d]: weight must not be negative", prefix, ti)
		}
		if _, ok := seen[parsed.String()]; ok {
			v.add("%s.targets[%d]: duplicate target %q", prefix, ti, t.URL)
		}
		seen[parsed.String()] = struct{}{}
	}

	tr := u.Transport
	if tr.MaxIdleConns < 0 || tr.MaxIdleConnsPerHost < 0 || tr.MaxConnsPerHost < 0 {
		v.add("%s.transport: connection counts must not be negative", prefix)
	}
	if tr.IdleConnTimeout.Duration < 0 || tr.DialTimeout.Duration <= 0 ||
		tr.ResponseHeaderTimeout.Duration < 0 || tr.TLSHandshakeTimeout.Duration < 0 {
		v.add("%s.transport: invalid timeout values", prefix)
	}

	h := u.HealthCheck
	if !strings.HasPrefix(h.Path, "/") {
		v.add("%s.health_check.path must start with /", prefix)
	}
	if h.Interval.Duration <= 0 || h.Timeout.Duration <= 0 {
		v.add("%s.health_check: interval and timeout must be positive", prefix)
	}
	if h.HealthyThreshold < 1 || h.UnhealthyThreshold < 1 {
		v.add("%s.health_check: thresholds must be at least 1", prefix)
	}
	if h.ExpectedStatusMin < 100 || h.ExpectedStatusMax > 599 || h.ExpectedStatusMin > h.ExpectedStatusMax {
		v.add("%s.health_check: invalid expected status range %d-%d", prefix, h.ExpectedStatusMin, h.ExpectedStatusMax)
	}

	b := u.Breaker
	if b.IsEnabled() {
		if b.FailureThreshold < 1 {
			v.add("%s.breaker.failure_threshold must be at least 1", prefix)
		}
		if b.FailureRatio <= 0 || b.FailureRatio > 1 {
			v.add("%s.breaker.failure_ratio must be in (0,1]", prefix)
		}
		if b.MinRequests < 1 {
			v.add("%s.breaker.min_requests must be at least 1", prefix)
		}
		if b.Window.Duration <= 0 || b.Buckets < 1 || b.Buckets > 100 {
			v.add("%s.breaker: window must be positive and buckets between 1 and 100", prefix)
		}
		if b.OpenTimeout.Duration <= 0 || b.HalfOpenMaxProbes < 1 || b.HalfOpenSuccesses < 1 {
			v.add("%s.breaker: open_timeout, half_open_max_probes and half_open_successes must be positive", prefix)
		}
	}

	r := u.Retry
	if r.IsEnabled() {
		if r.MaxAttempts < 1 || r.MaxAttempts > 10 {
			v.add("%s.retry.max_attempts must be between 1 and 10", prefix)
		}
		if r.BaseBackoff.Duration < 0 || r.MaxBackoff.Duration < 0 || r.BaseBackoff.Duration > r.MaxBackoff.Duration {
			v.add("%s.retry: backoff values are invalid", prefix)
		}
		if r.BodyBufferBytes < 1 {
			v.add("%s.retry.body_buffer_bytes must be positive", prefix)
		}
		if r.BudgetPercent < 1 || r.BudgetPercent > 100 {
			v.add("%s.retry.budget_percent must be between 1 and 100", prefix)
		}
		for _, reason := range r.On {
			if reason != "connection_error" && reason != "timeout" {
				code, err := strconv.Atoi(reason)
				if err != nil || code < 500 || code > 599 {
					v.add("%s.retry.on: unknown reason %q", prefix, reason)
				}
			}
		}
		for _, m := range r.Methods {
			if !validMethod(m) {
				v.add("%s.retry.methods: invalid method %q", prefix, m)
			}
		}
	}
}

func (c *Config) validateRoute(v *ValidationError, i int, r *Route, upstreams map[string]struct{}) {
	prefix := fmt.Sprintf("routes[%d] (%s)", i, r.Name)
	if r.Upstream == "" {
		v.add("%s: upstream is required", prefix)
	} else if _, ok := upstreams[r.Upstream]; !ok {
		v.add("%s: unknown upstream %q", prefix, r.Upstream)
	}

	if !strings.HasPrefix(r.Match.PathPrefix, "/") {
		v.add("%s.match.path_prefix must start with /", prefix)
	} else if path.Clean(r.Match.PathPrefix) != r.Match.PathPrefix {
		v.add("%s.match.path_prefix must be normalized (no //, ., or .. segments)", prefix)
	}
	if err := validateHostPattern(r.Match.Host); err != nil {
		v.add("%s.match.host: %v", prefix, err)
	}
	for _, m := range r.Match.Methods {
		if !validMethod(m) {
			v.add("%s.match.methods: invalid method %q", prefix, m)
		}
	}

	if r.StripPrefix != "" {
		if !strings.HasPrefix(r.StripPrefix, "/") {
			v.add("%s.strip_prefix must start with /", prefix)
		} else if r.StripPrefix != "/" {
			if !strings.HasPrefix(r.Match.PathPrefix, r.StripPrefix) {
				v.add("%s.strip_prefix %q is not a prefix of path_prefix %q", prefix, r.StripPrefix, r.Match.PathPrefix)
			} else if len(r.Match.PathPrefix) > len(r.StripPrefix) && r.Match.PathPrefix[len(r.StripPrefix)] != '/' {
				v.add("%s.strip_prefix %q does not end on a path boundary", prefix, r.StripPrefix)
			}
		}
	}

	if r.Timeout.Duration <= 0 {
		v.add("%s.timeout must be positive", prefix)
	}
	if r.MaxBodyBytes <= 0 {
		v.add("%s.max_body_bytes must be positive", prefix)
	}

	if r.Auth.Required {
		if len(r.Auth.Methods) == 0 {
			v.add("%s.auth.methods is required when auth.required is true", prefix)
		}
	}
	for _, m := range r.Auth.Methods {
		if m != "jwt" && m != "api_key" {
			v.add("%s.auth.methods: unknown method %q", prefix, m)
		}
	}
	if r.Auth.Match != "any" && r.Auth.Match != "all" {
		v.add("%s.auth.match must be any or all", prefix)
	}
	if !r.Auth.Required && (len(r.Auth.Scopes) > 0 || len(r.Auth.Roles) > 0) {
		v.add("%s.auth.scopes/roles require auth.required: true", prefix)
	}

	if rl := r.RateLimit; rl != nil {
		if rl.Capacity < 1 {
			v.add("%s.rate_limit.capacity must be at least 1", prefix)
		}
		if rl.RefillPerSec <= 0 {
			v.add("%s.rate_limit.refill_per_sec must be positive", prefix)
		}
		switch rl.Key {
		case "ip", "user", "api_key":
		case "header":
			if strings.TrimSpace(rl.Header) == "" {
				v.add("%s.rate_limit.header is required when key is header", prefix)
			}
		default:
			v.add("%s.rate_limit.key must be ip, user, api_key or header", prefix)
		}
	}
}

func (c *Config) validateRouteTies(v *ValidationError) {
	type ref struct {
		index   int
		methods map[string]struct{}
	}
	groups := make(map[string][]ref)
	for i := range c.Routes {
		r := &c.Routes[i]
		key := r.Match.Host + "\x00" + r.Match.PathPrefix
		var methods map[string]struct{}
		if len(r.Match.Methods) > 0 {
			methods = make(map[string]struct{}, len(r.Match.Methods))
			for _, m := range r.Match.Methods {
				methods[m] = struct{}{}
			}
		}
		groups[key] = append(groups[key], ref{index: i, methods: methods})
	}
	for key, refs := range groups {
		if len(refs) < 2 {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		host := parts[0]
		if host == "" {
			host = "*"
		}
		for a := 0; a < len(refs); a++ {
			for b := a + 1; b < len(refs); b++ {
				if methodsOverlap(refs[a].methods, refs[b].methods) {
					v.add("routes %q and %q have an ambiguous match (host %q, prefix %q); make method sets disjoint or add a longer prefix",
						c.Routes[refs[a].index].Name, c.Routes[refs[b].index].Name, host, parts[1])
				}
			}
		}
	}
}

func methodsOverlap(a, b map[string]struct{}) bool {
	if a == nil || b == nil {
		return true
	}
	for m := range a {
		if _, ok := b[m]; ok {
			return true
		}
	}
	return false
}

func (c *Config) validateAuthAvailability(v *ValidationError) {
	jwtReady := len(c.Auth.JWT.Algorithms) > 0
	keyReady := len(c.Auth.APIKeys) > 0
	for i := range c.Routes {
		r := &c.Routes[i]
		if !r.Auth.Required {
			continue
		}
		usable := false
		for _, m := range r.Auth.Methods {
			switch m {
			case "jwt":
				usable = usable || jwtReady
			case "api_key":
				usable = usable || keyReady
			}
		}
		if !usable {
			v.add("routes[%d] (%s): no configured authentication method can satisfy this route (jwt configured: %v, api keys configured: %v)",
				i, r.Name, jwtReady, keyReady)
		}
	}
}

func validateCIDROrIP(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty value")
	}
	if strings.Contains(raw, "/") {
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", raw, err)
		}
		return nil
	}
	if ip := net.ParseIP(strings.Trim(raw, "[]")); ip == nil {
		return fmt.Errorf("invalid IP %q", raw)
	}
	return nil
}

func validateHostPattern(host string) error {
	if host == "" {
		return nil
	}
	if strings.ContainsAny(host, "/\\ \t\r\n:") {
		return fmt.Errorf("host must be a DNS name without scheme, path or port")
	}
	if host == "*" {
		return fmt.Errorf("use *.example.com or omit host, not a bare *")
	}
	if strings.Contains(host, "*") {
		if !strings.HasPrefix(host, "*.") || strings.Count(host, "*") != 1 || len(host) <= 2 {
			return fmt.Errorf("wildcard host must have the form *.example.com")
		}
	}
	return nil
}

func validMethod(m string) bool {
	if m == "" {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		}
		return false
	}
	return true
}
