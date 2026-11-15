package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

func TestJSONLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(config.Log{Level: "debug", Format: "json"}, "1.2.3", &buf)
	logger.Debug("hello", slog.String("key", "value"))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("json: %v (%s)", err, buf.String())
	}
	if entry["level"] != "debug" || entry["msg"] != "hello" || entry["key"] != "value" {
		t.Fatalf("entry = %v", entry)
	}
	if entry["service"] != "gateway" || entry["version"] != "1.2.3" {
		t.Fatalf("service fields = %v", entry)
	}
	ts, ok := entry["time"].(string)
	if !ok || !strings.HasSuffix(ts, "Z") {
		t.Fatalf("time = %v", entry["time"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("time parse: %v", err)
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(config.Log{Level: "info", Format: "json"}, "", &buf)
	logger.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug not filtered: %s", buf.String())
	}
	logger.Info("shown")
	if !strings.Contains(buf.String(), `"msg":"shown"`) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestTextLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(config.Log{Level: "warn", Format: "text"}, "dev", &buf)
	logger.Warn("careful", slog.Int("n", 3))
	line := buf.String()
	for _, want := range []string{"level=warn", "msg=careful", "n=3", "service=gateway", "version=dev"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
}

func TestNewLoggerDefaults(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(config.Log{}, "", &buf)
	logger.Info("default")
	if !strings.Contains(buf.String(), `"level":"info"`) {
		t.Fatalf("log = %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"version":"dev"`) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		"info":     slog.LevelInfo,
		"":         slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWithRequestID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	if got := WithRequestID(nil, "x"); got != nil {
		t.Fatal("nil logger changed")
	}
	if got := WithRequestID(base, ""); got != base {
		t.Fatal("empty ID should return the same logger")
	}
	WithRequestID(base, "req-1").Info("line")
	if !strings.Contains(buf.String(), `"request_id":"req-1"`) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	Discard().Error("dropped")
}
