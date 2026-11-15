package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Parth-Sohal/api-gateway-routing/internal/balancer"
	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/httpx"
)

var (
	errBodyTooLarge = errors.New("proxy: request body too large")
	errReadBody     = errors.New("proxy: cannot read request body")
)

// requestBody retains a bounded prefix (or the whole body when it fits) so a
// retry can replay it. Anything larger streams once and is never replayed.
type requestBody struct {
	data       []byte
	stream     io.ReadCloser
	length     int64
	replayable bool
	used       bool
}

func (b *requestBody) open() (io.ReadCloser, int64, bool) {
	if b == nil {
		return nil, 0, true
	}
	if b.stream != nil {
		if b.used {
			return nil, 0, false
		}
		b.used = true
		return b.stream, b.length, true
	}
	if len(b.data) == 0 {
		return nil, 0, true
	}
	return io.NopCloser(bytes.NewReader(b.data)), int64(len(b.data)), true
}

func (b *requestBody) getBody() func() (io.ReadCloser, error) {
	if b == nil || !b.replayable || len(b.data) == 0 {
		return nil
	}
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b.data)), nil
	}
}

// prepareRequestBody enforces the route body limit and buffers only when
// retries could use the bytes. w is needed by http.MaxBytesReader.
func prepareRequestBody(w http.ResponseWriter, r *http.Request, policy retryPolicy, willRetry bool, maxBody int64) (*requestBody, error) {
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	if r.Body == nil || r.Body == http.NoBody {
		return &requestBody{replayable: true}, nil
	}
	if r.ContentLength > maxBody {
		_ = r.Body.Close()
		return nil, errBodyTooLarge
	}

	if !willRetry {
		stream := http.MaxBytesReader(w, r.Body, maxBody)
		return &requestBody{stream: stream, length: r.ContentLength, replayable: false}, nil
	}

	limit := policy.bodyBuffer
	if limit <= 0 {
		limit = defaultBodyBuffer
	}
	if limit > maxBody {
		limit = maxBody
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		_ = r.Body.Close()
		return nil, fmt.Errorf("%w: %w", errReadBody, err)
	}
	if int64(len(data)) > limit {
		// The body is larger than the replay buffer: forward the bytes we
		// already hold plus the rest, without retry support.
		stream := &readerCloser{
			Reader: io.MultiReader(bytes.NewReader(data), r.Body),
			Closer: r.Body,
		}
		return &requestBody{
			stream:     http.MaxBytesReader(w, stream, maxBody),
			length:     r.ContentLength,
			replayable: false,
		}, nil
	}
	if int64(len(data)) > maxBody {
		_ = r.Body.Close()
		return nil, errBodyTooLarge
	}
	// Drain to EOF already happened inside LimitReader; close the server body.
	_ = r.Body.Close()
	return &requestBody{data: data, length: int64(len(data)), replayable: true}, nil
}

type readerCloser struct {
	io.Reader
	io.Closer
}

func buildUpstreamRequest(ctx context.Context, r *http.Request, route *config.Route, target *balancer.Target, body io.ReadCloser, contentLength int64, getBody func() (io.ReadCloser, error)) (*http.Request, error) {
	u := targetURL(target.URL, r, route.StripPrefix)
	req := r.Clone(ctx)
	req.URL = u
	req.RequestURI = ""
	req.Body = body
	req.ContentLength = contentLength
	req.GetBody = getBody
	req.Trailer = nil
	req.TransferEncoding = nil
	req.Close = false
	req.Host = target.URL.Host
	if route.PreserveHost && r.Host != "" {
		req.Host = r.Host
	}
	prepareHeaders(req, r)
	req.Header.Del("Content-Length")
	if req.Body == nil {
		req.ContentLength = 0
	}
	return req, nil
}

func targetURL(base *url.URL, r *http.Request, stripPrefix string) *url.URL {
	u := *base
	u.Path = joinPaths(base.Path, stripPath(r.URL.Path, stripPrefix))
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	u.ForceQuery = r.URL.ForceQuery
	u.Fragment = ""
	u.User = nil
	return &u
}

func stripPath(path, prefix string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return path
	}
	if path == prefix {
		return "/"
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):]
	}
	return path
}

func joinPaths(base, req string) string {
	if base == "" {
		if req == "" {
			return "/"
		}
		if !strings.HasPrefix(req, "/") {
			return "/" + req
		}
		return req
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	base = strings.TrimSuffix(base, "/")
	if req == "" {
		return base + "/"
	}
	if !strings.HasPrefix(req, "/") {
		req = "/" + req
	}
	return base + req
}

func prepareHeaders(req *http.Request, r *http.Request) {
	h := req.Header
	removeHopByHop(h)

	for _, name := range [...]string{
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip",
	} {
		h.Del(name)
	}

	info := httpx.InfoFrom(r.Context())
	clientIP := ""
	requestID := ""
	if info != nil {
		clientIP = info.ClientIP
		requestID = info.RequestID
	}
	if clientIP == "" {
		clientIP = httpx.ClientIP(r, httpx.TrustedProxies{})
	}
	if clientIP != "" {
		h.Set("X-Forwarded-For", clientIP)
		h.Set("X-Real-Ip", clientIP)
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
	h.Set("X-Forwarded-Host", r.Host)
	if requestID != "" {
		h.Set(httpx.HeaderRequestID, requestID)
	}
}

// removeHopByHop strips headers that are meaningful only for one connection.
// It also removes every header nominated by Connection.
func removeHopByHop(h http.Header) {
	for _, conn := range h.Values("Connection") {
		for _, token := range strings.Split(conn, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

var hopByHopHeaders = [...]string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyResponseHeaders copies an upstream response header set without mutating
// it. Hop-by-hop headers and any upstream request-ID spoofing are dropped.
func copyResponseHeaders(dst, src http.Header, info *httpx.Info) {
	skip := make(map[string]struct{}, len(hopByHopHeaders)+4)
	for _, conn := range src.Values("Connection") {
		for _, token := range strings.Split(conn, ",") {
			if token = strings.TrimSpace(token); token != "" {
				skip[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	for _, name := range hopByHopHeaders {
		skip[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	requestIDKey := http.CanonicalHeaderKey(httpx.HeaderRequestID)
	for k, vals := range src {
		key := http.CanonicalHeaderKey(k)
		if _, drop := skip[key]; drop || key == requestIDKey {
			continue
		}
		dst[key] = append([]string(nil), vals...)
	}
	if info != nil && info.RequestID != "" {
		dst.Set(httpx.HeaderRequestID, info.RequestID)
	}
}
