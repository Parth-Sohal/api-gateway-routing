package httpx

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Stable machine-readable error codes returned by the gateway.
const (
	CodeRouteNotFound       = "route_not_found"
	CodeMethodNotAllowed    = "method_not_allowed"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodePayloadTooLarge     = "payload_too_large"
	CodeRateLimited         = "rate_limited"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeNoHealthyTargets    = "no_healthy_targets"
	CodeLimiterUnavailable  = "limiter_unavailable"
	CodeBadGateway          = "bad_gateway"
	CodeGatewayTimeout      = "gateway_timeout"
	CodeInternalError       = "internal_error"
	CodeOverloaded          = "gateway_overloaded"
	CodeInvalidRequest      = "invalid_request"
)

// Error is a gateway-generated HTTP error. Upstream response bodies are never
// replaced with this envelope; it is only for errors produced by the gateway.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Err        error
}

// NewError builds a gateway error.
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Error implements error.
func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

// Unwrap supports errors.Is/As on the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

// WriteError writes the standard JSON error envelope. It returns false when a
// response was already started (for example a panic after streaming began);
// callers must not write anything else in that case.
func WriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	if responseStarted(w) {
		return false
	}

	var ge *Error
	if !errors.As(err, &ge) || ge == nil {
		ge = &Error{Status: http.StatusInternalServerError, Code: CodeInternalError, Message: "Internal server error."}
	}
	status := ge.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	code := ge.Code
	if code == "" {
		code = CodeInternalError
	}
	message := ge.Message
	if message == "" {
		message = http.StatusText(status)
	}

	if info := InfoFrom(r.Context()); info != nil {
		info.ErrorCode = code
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if ge.RetryAfter > 0 {
		secs := int(math.Ceil(ge.RetryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		h.Set("Retry-After", strconv.Itoa(secs))
	}

	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestID = RequestID(r.Context())

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	return true
}

// responseStarted walks the writer's Unwrap chain looking for the recorder's
// Written marker. Writers that do not expose it are assumed unwritten.
func responseStarted(w http.ResponseWriter) bool {
	for {
		if s, ok := w.(interface{ Written() bool }); ok {
			return s.Written()
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}
