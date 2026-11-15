// Package router matches incoming requests to configured routes. Precedence is
// longest path prefix, then host specificity (exact > wildcard > any), then
// HTTP method. Ambiguous route tables are rejected at config validation.
package router

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// Route is a compiled config route.
type Route struct {
	Config   *config.Route
	prefix   string
	host     string
	wildcard bool
	methods  map[string]struct{}
}

// Router is an immutable route table.
type Router struct {
	routes []*Route
}

// MatchError describes a routing rejection with the HTTP status the gateway
// should return.
type MatchError struct {
	Status  int
	Allowed []string
}

func (e *MatchError) Error() string {
	if e.Status == http.StatusMethodNotAllowed {
		return "method not allowed"
	}
	return "route not found"
}

// New compiles routes. It expects config validation to have run, but still
// rejects structurally unusable routes.
func New(routes []config.Route) (*Router, error) {
	compiled := make([]*Route, 0, len(routes))
	for i := range routes {
		cfg := &routes[i]
		if cfg.Name == "" {
			return nil, fmt.Errorf("route[%d]: empty name", i)
		}
		if !strings.HasPrefix(cfg.Match.PathPrefix, "/") {
			return nil, fmt.Errorf("route %q: path prefix must start with /", cfg.Name)
		}
		r := &Route{
			Config: cfg,
			prefix: cfg.Match.PathPrefix,
			host:   strings.ToLower(strings.TrimSpace(cfg.Match.Host)),
		}
		if strings.HasPrefix(r.host, "*.") {
			r.wildcard = true
		}
		if len(cfg.Match.Methods) > 0 {
			r.methods = make(map[string]struct{}, len(cfg.Match.Methods))
			for _, m := range cfg.Match.Methods {
				r.methods[m] = struct{}{}
			}
		}
		compiled = append(compiled, r)
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		a, b := compiled[i], compiled[j]
		if len(a.prefix) != len(b.prefix) {
			return len(a.prefix) > len(b.prefix)
		}
		if sa, sb := hostSpecificity(a), hostSpecificity(b); sa != sb {
			return sa > sb
		}
		if a.host != b.host {
			return a.host < b.host
		}
		return a.Config.Name < b.Config.Name
	})
	return &Router{routes: compiled}, nil
}

// Match returns the best route for r. A 404 MatchError means nothing matched;
// a 405 means the path/host matched but no route accepted the method.
func (rt *Router) Match(r *http.Request) (*Route, error) {
	host := requestHost(r.Host)
	path := r.URL.Path
	if path == "" {
		path = "/"
	}

	bestLen := -1
	for _, route := range rt.routes {
		if !route.matchesHost(host) || !pathPrefixMatch(path, route.prefix) {
			continue
		}
		if len(route.prefix) > bestLen {
			bestLen = len(route.prefix)
		}
	}
	if bestLen < 0 {
		return nil, &MatchError{Status: http.StatusNotFound}
	}

	// Among the longest prefix, prefer exact host, then wildcard, then any.
	bestSpec := -1
	for _, route := range rt.routes {
		if len(route.prefix) != bestLen || !route.matchesHost(host) {
			continue
		}
		if s := hostSpecificity(route); s > bestSpec {
			bestSpec = s
		}
	}

	var candidates []*Route
	for _, route := range rt.routes {
		if len(route.prefix) != bestLen || hostSpecificity(route) != bestSpec || !route.matchesHost(host) {
			continue
		}
		candidates = append(candidates, route)
	}

	method := r.Method
	for _, route := range candidates {
		if route.allowsMethod(method) {
			return route, nil
		}
	}

	allowedSet := make(map[string]struct{})
	for _, route := range candidates {
		for m := range route.methods {
			allowedSet[m] = struct{}{}
		}
	}
	allowed := make([]string, 0, len(allowedSet))
	for m := range allowedSet {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return nil, &MatchError{Status: http.StatusMethodNotAllowed, Allowed: allowed}
}

// Routes returns the compiled route table in precedence order.
func (rt *Router) Routes() []*Route { return rt.routes }

// AllowedMethods returns the route's method set, or nil for all methods.
func (r *Route) AllowedMethods() []string {
	if len(r.methods) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.methods))
	for m := range r.methods {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (r *Route) allowsMethod(method string) bool {
	if len(r.methods) == 0 {
		return true
	}
	_, ok := r.methods[method]
	return ok
}

func (r *Route) matchesHost(host string) bool {
	if r.host == "" {
		return true
	}
	if !r.wildcard {
		return r.host == host
	}
	suffix := r.host[1:] // ".example.com"
	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}

func hostSpecificity(r *Route) int {
	if r.host == "" {
		return 0
	}
	if r.wildcard {
		return 1
	}
	return 2
}

// pathPrefixMatch enforces a segment boundary: /api matches /api and
// /api/users but not /apix.
func pathPrefixMatch(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

func requestHost(hostport string) string {
	h := strings.TrimSpace(strings.ToLower(hostport))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(h, ".")
}
