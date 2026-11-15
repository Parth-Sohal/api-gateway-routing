package auth

import (
	"net/http"
	"strings"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

// Authenticator verifies JWT and API-key credentials. It is immutable after
// construction and safe for concurrent use.
type Authenticator struct {
	jwt  *jwtVerifier
	keys *apiKeyVerifier
}

// New builds an authenticator from config. Secrets are read from the
// environment or mounted files here, never stored in the config object.
func New(cfg config.Auth) (*Authenticator, error) {
	jwtVerifier, err := newJWTVerifier(cfg.JWT)
	if err != nil {
		return nil, err
	}
	keyVerifier, err := newAPIKeyVerifier(cfg.APIKeys)
	if err != nil {
		return nil, err
	}
	return &Authenticator{jwt: jwtVerifier, keys: keyVerifier}, nil
}

type credentials struct {
	bearer    string
	apiKey    string
	present   bool
	ambiguous bool
}

// Authenticate applies the route's auth policy. When auth is optional and no
// credentials are supplied it returns an anonymous principal; supplied but
// invalid credentials are always rejected.
func (a *Authenticator) Authenticate(r *http.Request, policy config.RouteAuth) (*Principal, error) {
	creds := extractCredentials(r)
	if creds.ambiguous {
		return nil, authError(ReasonInvalid, "conflicting credentials supplied")
	}
	if !creds.present {
		if policy.Required {
			return nil, authError(ReasonMissing, "authentication required")
		}
		return &Principal{Kind: "anonymous"}, nil
	}

	allowJWT, allowKey := allowedMethods(policy.Methods)
	var firstErr error
	try := func(p *Principal, err error) (*Principal, bool) {
		if err == nil {
			return p, true
		}
		if firstErr == nil || errorRank(err) > errorRank(firstErr) {
			firstErr = err
		}
		return nil, false
	}

	if creds.bearer != "" {
		if strings.HasPrefix(creds.bearer, "gw_") {
			if allowKey {
				if p, ok := try(a.verifyKey(creds.bearer)); ok {
					return p, nil
				}
			} else if allowJWT {
				if p, ok := try(a.verifyJWT(creds.bearer)); ok {
					return p, nil
				}
			} else {
				firstErr = authError(ReasonInvalid, "credential type is not allowed for this route")
			}
		} else if allowJWT {
			if p, ok := try(a.verifyJWT(creds.bearer)); ok {
				return p, nil
			}
		} else {
			firstErr = authError(ReasonInvalid, "bearer tokens are not allowed for this route")
		}
	}

	if creds.apiKey != "" {
		if allowKey {
			if p, ok := try(a.verifyKey(creds.apiKey)); ok {
				return p, nil
			}
		} else if firstErr == nil {
			firstErr = authError(ReasonInvalid, "API keys are not allowed for this route")
		}
	}

	if firstErr == nil {
		firstErr = authError(ReasonInvalid, "no usable credentials")
	}
	return nil, firstErr
}

func (a *Authenticator) verifyJWT(token string) (*Principal, error) {
	if a.jwt == nil {
		return nil, authError(ReasonInvalid, "JWT authentication is not configured")
	}
	return a.jwt.verify(token)
}

func (a *Authenticator) verifyKey(key string) (*Principal, error) {
	if a.keys == nil {
		return nil, authError(ReasonInvalid, "API-key authentication is not configured")
	}
	return a.keys.verify(key)
}

func allowedMethods(methods []string) (jwt, apiKey bool) {
	if len(methods) == 0 {
		return true, true
	}
	for _, m := range methods {
		switch m {
		case "jwt":
			jwt = true
		case "api_key":
			apiKey = true
		}
	}
	return jwt, apiKey
}

func extractCredentials(r *http.Request) credentials {
	var c credentials
	authHeaders := r.Header.Values("Authorization")
	if len(authHeaders) > 1 {
		c.present = true
		c.ambiguous = true
		return c
	}
	authHeader := ""
	if len(authHeaders) == 1 {
		authHeader = strings.TrimSpace(authHeaders[0])
	}
	headerKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if headerKey != "" {
		c.present = true
		c.apiKey = headerKey
	}

	if authHeader != "" {
		c.present = true
		scheme, value, ok := strings.Cut(authHeader, " ")
		if !ok {
			c.ambiguous = true
			return c
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(scheme)) {
		case "bearer":
			c.bearer = value
		case "apikey", "api-key":
			if c.apiKey != "" && c.apiKey != value {
				c.ambiguous = true
				return c
			}
			c.apiKey = value
		default:
			c.ambiguous = true
			return c
		}
	}

	if c.bearer != "" && strings.HasPrefix(c.bearer, "gw_") {
		if c.apiKey != "" && c.apiKey != c.bearer {
			c.ambiguous = true
			return c
		}
		c.apiKey = c.bearer
		c.bearer = ""
	}
	if c.apiKey == "" && c.bearer == "" {
		c.present = false
	}
	return c
}

func errorRank(err error) int {
	ae, ok := err.(*Error)
	if !ok {
		return 0
	}
	switch ae.Reason {
	case ReasonRevoked:
		return 5
	case ReasonExpired:
		return 4
	case ReasonNotYetValid:
		return 3
	case ReasonWrongAudience, ReasonWrongIssuer:
		return 2
	case ReasonInvalid:
		return 1
	default:
		return 0
	}
}
