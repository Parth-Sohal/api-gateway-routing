package httpx

import (
	"errors"
	"path"
	"strings"
)

// ErrInvalidPath is returned for request targets that cannot be normalized
// safely.
var ErrInvalidPath = errors.New("invalid request path")

// NormalizePath cleans a URL path before routing. It collapses duplicate
// slashes and resolves dot segments so that "/api/../admin" cannot match an
// admin route by accident. The returned path is what the router, rate limiter
// and proxy should use consistently.
func NormalizePath(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", ErrInvalidPath
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return "", ErrInvalidPath
		}
	}
	clean := path.Clean(p)
	if !strings.HasPrefix(clean, "/") {
		return "", ErrInvalidPath
	}
	return clean, nil
}
