// Package auth verifies JWTs and API keys, and checks per-route scopes/roles.
package auth

import (
	"context"
	"fmt"
)

// Principal is the authenticated caller identity.
type Principal struct {
	ID     string
	Owner  string
	Kind   string // jwt | api_key | anonymous
	Scopes []string
	Roles  []string
}

// IsAnonymous reports whether p represents an unauthenticated caller.
func (p *Principal) IsAnonymous() bool {
	return p == nil || p.Kind == "anonymous"
}

// LogID returns a low-cardinality identity for access logs, e.g. user:42 or
// key:demo. Anonymous callers have no log identity.
func (p *Principal) LogID() string {
	if p.IsAnonymous() {
		return ""
	}
	switch p.Kind {
	case "api_key":
		return "key:" + p.ID
	default:
		return "user:" + p.ID
	}
}

// RateLimitIdentity returns the identity used for user/api_key limits.
func (p *Principal) RateLimitIdentity(keyType string) string {
	if p.IsAnonymous() {
		return ""
	}
	switch keyType {
	case "api_key":
		return p.ID
	case "user":
		if p.Owner != "" {
			return p.Owner
		}
		return p.ID
	default:
		return p.ID
	}
}

// Reason classifies an authentication failure for metrics and logging.
type Reason string

const (
	ReasonMissing       Reason = "missing"
	ReasonInvalid       Reason = "invalid"
	ReasonExpired       Reason = "expired"
	ReasonNotYetValid   Reason = "not_yet_valid"
	ReasonWrongAudience Reason = "wrong_audience"
	ReasonWrongIssuer   Reason = "wrong_issuer"
	ReasonRevoked       Reason = "revoked"
	ReasonForbidden     Reason = "forbidden"
)

// Error is a typed authentication/authorization error.
type Error struct {
	Reason Reason
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Reason) + ": " + e.Err.Error()
	}
	return string(e.Reason)
}

func (e *Error) Unwrap() error { return e.Err }

func authError(reason Reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

type principalKey struct{}

// WithPrincipal attaches a principal to ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal attached to ctx.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

// Authorize checks required scopes and roles. Each non-empty requirement list
// must pass; Match selects any/all within a list.
func Authorize(p *Principal, required bool, scopes, roles []string, match string) error {
	if !required {
		return nil
	}
	if p.IsAnonymous() {
		return authError(ReasonForbidden, "authentication required")
	}
	if !checkList(p.Scopes, scopes, match) {
		return authError(ReasonForbidden, "missing required scope")
	}
	if !checkList(p.Roles, roles, match) {
		return authError(ReasonForbidden, "missing required role")
	}
	return nil
}

func checkList(have, want []string, match string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, v := range have {
		set[v] = struct{}{}
	}
	if match == "all" {
		for _, v := range want {
			if _, ok := set[v]; !ok {
				return false
			}
		}
		return true
	}
	for _, v := range want {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}
