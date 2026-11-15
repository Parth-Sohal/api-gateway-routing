package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRequestID(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if len(id) != 26 {
			t.Fatalf("len = %d, want 26 (%q)", len(id), id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = struct{}{}
		for _, c := range id {
			if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", c) {
				t.Fatalf("invalid character %q in %q", c, id)
			}
		}
	}
}

func TestSanitizeRequestID(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"abc-123_X.Y:Z", "abc-123_X.Y:Z", true},
		{"  abc  ", "abc", true},
		{"", "", false},
		{strings.Repeat("a", 129), "", false},
		{"bad\nvalue", "", false},
		{"has space", "", false},
	}
	for _, tc := range tests {
		got, ok := SanitizeRequestID(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("SanitizeRequestID(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWriteError(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	info := &Info{RequestID: "req-1"}
	req = req.WithContext(WithInfo(req.Context(), info))
	rec := httptest.NewRecorder()

	ok := WriteError(rec, req, &Error{
		Status:     429,
		Code:       CodeRateLimited,
		Message:    "Too many requests.",
		RetryAfter: 1200 * 1000 * 1000, // 1.2s
	})
	if !ok {
		t.Fatal("WriteError returned false for unwritten response")
	}
	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"rate_limited"`) || !strings.Contains(body, `"request_id":"req-1"`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if info.ErrorCode != CodeRateLimited {
		t.Fatalf("info.ErrorCode = %q", info.ErrorCode)
	}
}

func TestWriteErrorAlreadyWritten(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	base := httptest.NewRecorder()
	w := NewRecorder(base)
	w.WriteHeader(200)
	if WriteError(w, req, NewError(500, CodeInternalError, "x")) {
		t.Fatal("WriteError should refuse an already-started response")
	}
	if base.Body.Len() != 0 {
		t.Fatal("unexpected error body after response started")
	}
}

func TestWriteErrorUnknown(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	WriteError(rec, req, errString("boom"))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), CodeInternalError) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestRecorder(t *testing.T) {
	base := httptest.NewRecorder()
	rec := NewRecorder(base)
	if rec.Written() || rec.Status() != 0 || rec.Bytes() != 0 {
		t.Fatal("new recorder should be empty")
	}
	rec.WriteHeader(201)
	rec.WriteHeader(500)
	n, err := rec.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d,%v)", n, err)
	}
	if !rec.Written() || rec.Status() != 201 || rec.Bytes() != 5 {
		t.Fatalf("state = written:%v status:%d bytes:%d", rec.Written(), rec.Status(), rec.Bytes())
	}
	if base.Code != 201 || base.Body.String() != "hello" {
		t.Fatalf("underlying = %d %q", base.Code, base.Body.String())
	}
	if rec.Unwrap() != base {
		t.Fatal("Unwrap should return base writer")
	}
	rec.Flush() // must not panic
}

func TestRecorderImplicit200(t *testing.T) {
	rec := NewRecorder(httptest.NewRecorder())
	rec.Write([]byte("x"))
	if rec.Status() != 200 {
		t.Fatalf("status = %d", rec.Status())
	}
}
