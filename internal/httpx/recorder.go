package httpx

import (
	"net/http"
)

// Recorder wraps a ResponseWriter, remembering status, bytes written and
// whether the response has started. It also keeps Flusher available through
// Unwrap for ResponseController.
type Recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

// NewRecorder wraps w.
func NewRecorder(w http.ResponseWriter) *Recorder { return &Recorder{ResponseWriter: w} }

// WriteHeader records and forwards the status code once.
func (r *Recorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

// Write records and forwards the body.
func (r *Recorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Written reports whether headers/body have been sent.
func (r *Recorder) Written() bool { return r.wrote }

// Status returns the response status, or 0 if nothing was written.
func (r *Recorder) Status() int { return r.status }

// Bytes reports the number of body bytes written.
func (r *Recorder) Bytes() int64 { return r.bytes }

// Unwrap exposes the underlying writer to http.ResponseController and to
// responseStarted.
func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher for streaming responses.
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
