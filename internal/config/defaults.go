package config

import (
	"strings"
	"time"
)

// applyDefaults fills omitted fields with safe defaults. It is idempotent and
// may be called by Validate for callers that construct Config in Go.
func (c *Config) applyDefaults() {
	c.applyServerDefaults()
	c.applyAdminDefaults()
	c.applyMetricsDefaults()
	c.applyRedisDefaults()
	c.applyAuthDefaults()
	c.applyLogDefaults()

	for i := range c.Upstreams {
		c.applyUpstreamDefaults(&c.Upstreams[i])
	}
	for i := range c.Routes {
		c.applyRouteDefaults(&c.Routes[i])
	}
}

func (c *Config) applyServerDefaults() {
	s := &c.Server
	if s.Addr == "" {
		s.Addr = ":8080"
	}
	if s.ReadHeaderTimeout.Duration == 0 {
		s.ReadHeaderTimeout.Duration = 10 * time.Second
	}
	if s.ReadTimeout.Duration == 0 {
		s.ReadTimeout.Duration = 30 * time.Second
	}
	// WriteTimeout intentionally defaults to 0 so response streaming (SSE) is
	// not cut off. Operators can set a finite value for non-streaming traffic.
	if s.IdleTimeout.Duration == 0 {
		s.IdleTimeout.Duration = 90 * time.Second
	}
	if s.MaxHeaderBytes == 0 {
		s.MaxHeaderBytes = 16 << 10
	}
	if s.ShutdownGrace.Duration == 0 {
		s.ShutdownGrace.Duration = 15 * time.Second
	}
}

func (c *Config) applyAdminDefaults() {
	a := &c.Admin
	if a.Enabled == nil {
		v := true
		a.Enabled = &v
	}
	if a.Addr == "" {
		a.Addr = "127.0.0.1:9090"
	}
	if a.TokenEnv == "" {
		a.TokenEnv = "GATEWAY_ADMIN_TOKEN"
	}
	if a.ReadHeaderTimeout.Duration == 0 {
		a.ReadHeaderTimeout.Duration = 5 * time.Second
	}
	if a.ShutdownGrace.Duration == 0 {
		a.ShutdownGrace.Duration = 10 * time.Second
	}
}

func (c *Config) applyMetricsDefaults() {
	m := &c.Metrics
	if m.Enabled == nil {
		v := true
		m.Enabled = &v
	}
	if m.Addr == "" {
		m.Addr = "127.0.0.1:9100"
	}
	if m.Path == "" {
		m.Path = "/metrics"
	}
}

func (c *Config) applyRedisDefaults() {
	r := &c.Redis
	if r.Addr == "" {
		r.Addr = "127.0.0.1:6379"
	}
	if r.PoolSize == 0 {
		r.PoolSize = 64
	}
	if r.MinIdleConns == 0 {
		r.MinIdleConns = 8
	}
	if r.DialTimeout.Duration == 0 {
		r.DialTimeout.Duration = 2 * time.Second
	}
	if r.ReadTimeout.Duration == 0 {
		r.ReadTimeout.Duration = 500 * time.Millisecond
	}
	if r.WriteTimeout.Duration == 0 {
		r.WriteTimeout.Duration = 500 * time.Millisecond
	}
	if r.OpTimeout.Duration == 0 {
		r.OpTimeout.Duration = 20 * time.Millisecond
	}
	if r.FailOpen == nil {
		v := true
		r.FailOpen = &v
	}
}

func (c *Config) applyAuthDefaults() {
	j := &c.Auth.JWT
	if len(j.Algorithms) == 0 {
		if j.HS256SecretEnv != "" || j.HS256SecretFile != "" {
			j.Algorithms = append(j.Algorithms, "HS256")
		}
		if j.RS256PublicKeyFile != "" {
			j.Algorithms = append(j.Algorithms, "RS256")
		}
	}
	j.Algorithms = canonicalUpperUnique(j.Algorithms)
	if j.Leeway.Duration == 0 {
		j.Leeway.Duration = time.Second
	}
	if j.ScopesClaim == "" {
		j.ScopesClaim = "scope"
	}
}

func (c *Config) applyLogDefaults() {
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	c.Log.Level = strings.ToLower(c.Log.Level)
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	c.Log.Format = strings.ToLower(c.Log.Format)
	if c.Log.AccessLog == nil {
		v := true
		c.Log.AccessLog = &v
	}
}

func (c *Config) applyUpstreamDefaults(u *Upstream) {
	u.Balancer = normalizeBalancer(u.Balancer)
	for i := range u.Targets {
		if u.Targets[i].Weight == 0 {
			u.Targets[i].Weight = 1
		}
	}

	t := &u.Transport
	if t.MaxIdleConns == 0 {
		t.MaxIdleConns = 1024
	}
	if t.MaxIdleConnsPerHost == 0 {
		t.MaxIdleConnsPerHost = 256
	}
	if t.IdleConnTimeout.Duration == 0 {
		t.IdleConnTimeout.Duration = 90 * time.Second
	}
	if t.DialTimeout.Duration == 0 {
		t.DialTimeout.Duration = time.Second
	}
	if t.ResponseHeaderTimeout.Duration == 0 {
		t.ResponseHeaderTimeout.Duration = 10 * time.Second
	}
	if t.TLSHandshakeTimeout.Duration == 0 {
		t.TLSHandshakeTimeout.Duration = 10 * time.Second
	}
	if t.ExpectContinueTimeout.Duration == 0 {
		t.ExpectContinueTimeout.Duration = time.Second
	}

	h := &u.HealthCheck
	if h.Path == "" {
		h.Path = "/healthz"
	}
	if h.Interval.Duration == 0 {
		h.Interval.Duration = 5 * time.Second
	}
	if h.Timeout.Duration == 0 {
		h.Timeout.Duration = time.Second
	}
	if h.HealthyThreshold == 0 {
		h.HealthyThreshold = 2
	}
	if h.UnhealthyThreshold == 0 {
		h.UnhealthyThreshold = 3
	}
	if h.ExpectedStatusMin == 0 {
		h.ExpectedStatusMin = 200
	}
	if h.ExpectedStatusMax == 0 {
		h.ExpectedStatusMax = 399
	}

	b := &u.Breaker
	if b.Enabled == nil {
		v := true
		b.Enabled = &v
	}
	if b.FailureThreshold == 0 {
		b.FailureThreshold = 5
	}
	if b.FailureRatio == 0 {
		b.FailureRatio = 0.5
	}
	if b.MinRequests == 0 {
		b.MinRequests = 20
	}
	if b.Window.Duration == 0 {
		b.Window.Duration = 10 * time.Second
	}
	if b.Buckets == 0 {
		b.Buckets = 10
	}
	if b.OpenTimeout.Duration == 0 {
		b.OpenTimeout.Duration = 5 * time.Second
	}
	if b.HalfOpenMaxProbes == 0 {
		b.HalfOpenMaxProbes = 3
	}
	if b.HalfOpenSuccesses == 0 {
		b.HalfOpenSuccesses = 2
	}

	r := &u.Retry
	if r.Enabled == nil {
		v := true
		r.Enabled = &v
	}
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 2
	}
	if len(r.On) == 0 {
		r.On = []string{"connection_error", "502", "503", "504"}
	}
	if len(r.Methods) == 0 {
		r.Methods = []string{"GET", "HEAD", "PUT", "DELETE", "OPTIONS"}
	}
	r.Methods = canonicalUpperUnique(r.Methods)
	if r.BaseBackoff.Duration == 0 {
		r.BaseBackoff.Duration = 50 * time.Millisecond
	}
	if r.MaxBackoff.Duration == 0 {
		r.MaxBackoff.Duration = time.Second
	}
	if r.BodyBufferBytes == 0 {
		r.BodyBufferBytes = 64 << 10
	}
	if r.BudgetPercent == 0 {
		r.BudgetPercent = 10
	}
}

func (c *Config) applyRouteDefaults(r *Route) {
	if r.Match.PathPrefix == "" {
		r.Match.PathPrefix = "/"
	}
	r.Match.PathPrefix = trimTrailingSlash(r.Match.PathPrefix)
	r.Match.Host = strings.ToLower(strings.TrimSpace(r.Match.Host))
	r.Match.Methods = canonicalUpperUnique(r.Match.Methods)
	for _, m := range r.Match.Methods {
		if m == "*" {
			r.Match.Methods = nil
			break
		}
	}

	if r.StripPrefix != "" {
		r.StripPrefix = trimTrailingSlash(r.StripPrefix)
	}

	if len(r.Auth.Methods) > 0 {
		out := make([]string, 0, len(r.Auth.Methods))
		for _, m := range r.Auth.Methods {
			out = append(out, normalizeAuthMethod(m))
		}
		r.Auth.Methods = uniqueStrings(out)
	} else if r.Auth.Required {
		r.Auth.Methods = []string{"jwt", "api_key"}
	}
	if r.Auth.Match == "" {
		r.Auth.Match = "any"
	}
	r.Auth.Match = strings.ToLower(r.Auth.Match)
	r.Auth.Scopes = uniqueStrings(r.Auth.Scopes)
	r.Auth.Roles = uniqueStrings(r.Auth.Roles)

	if r.RateLimit != nil {
		r.RateLimit.Key = strings.ToLower(strings.TrimSpace(r.RateLimit.Key))
		if r.RateLimit.Key == "" {
			r.RateLimit.Key = "ip"
		}
	}
	if r.Timeout.Duration == 0 {
		r.Timeout.Duration = 30 * time.Second
	}
	if r.MaxBodyBytes == 0 {
		r.MaxBodyBytes = 1 << 20
	}
}

func normalizeBalancer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "round_robin", "round-robin", "rr":
		return "round_robin"
	case "weighted", "weighted_round_robin", "weighted-round-robin", "wrr":
		return "weighted_round_robin"
	case "least_conn", "least-connections", "least_connections", "leastconn":
		return "least_conn"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func normalizeAuthMethod(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "apikey", "api-key", "api_key":
		return "api_key"
	case "jwt":
		return "jwt"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func canonicalUpperUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.ToUpper(strings.TrimSpace(v))
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

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
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

func trimTrailingSlash(s string) string {
	if len(s) > 1 && strings.HasSuffix(s, "/") {
		return strings.TrimRight(s, "/")
	}
	return s
}
