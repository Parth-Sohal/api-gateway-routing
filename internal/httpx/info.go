// Package httpx contains the small HTTP primitives shared by the gateway:
// request IDs and request-scoped info, the error envelope, client IP
// extraction, path normalization, header hygiene and a response recorder.
package httpx

import "context"

type infoKey struct{}

// Info is request-scoped metadata used by logging, metrics and error
// responses. It is written only by the goroutine serving the request and read
// by middleware after the handler returns.
type Info struct {
	RequestID string
	Route     string
	Upstream  string
	Target    string
	Principal string
	ClientIP  string
	ErrorCode string
	Retries   int
}

// WithInfo attaches info to ctx.
func WithInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, infoKey{}, info)
}

// InfoFrom returns the request info, or nil if none was attached.
func InfoFrom(ctx context.Context) *Info {
	info, _ := ctx.Value(infoKey{}).(*Info)
	return info
}

// RequestID returns the request ID attached to ctx, or "".
func RequestID(ctx context.Context) string {
	if info := InfoFrom(ctx); info != nil {
		return info.RequestID
	}
	return ""
}
