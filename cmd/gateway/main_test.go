package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
server:
  addr: 127.0.0.1:0
admin:
  enabled: false
metrics:
  enabled: false
log:
  access_log: false
upstreams:
  - name: api
    targets:
      - url: http://127.0.0.1:9001
routes:
  - name: api
    match:
      path_prefix: /
    upstream: api
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunValidate(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"-config", writeTempConfig(t, validConfig), "-validate"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "configuration valid") || !strings.Contains(out.String(), "1 routes") {
		t.Fatalf("stdout = %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunInvalidConfig(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"-config", writeTempConfig(t, "routes: [")}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "gateway:") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunMissingConfig(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"-config", filepath.Join(t.TempDir(), "absent.yaml")}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "read config") {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"-version"}, &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("exit = %d, stdout = %q", code, out.String())
	}
}

func TestRunFlagErrors(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"-nope"},
		{"extra"},
		{"-config", writeTempConfig(t, validConfig), "-log-level", "loud"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr %q)", args, code, errOut.String())
		}
	}
}

func TestRunLogLevelOverride(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"-config", writeTempConfig(t, validConfig), "-log-level", "DEBUG", "-validate"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
}
