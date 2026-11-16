package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewWatcherValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewWatcher("", nil, func() error { return nil }); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := NewWatcher("gateway.yaml", nil, nil); err == nil {
		t.Fatal("nil reload callback accepted")
	}
	w, err := NewWatcher(filepath.Join(t.TempDir(), "gateway.yaml"), nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !w.matches("/tmp/other/GATEWAY.YAML") {
		t.Fatal("watcher must match the config basename case-insensitively")
	}
	if w.matches("/tmp/other/other.yaml") {
		t.Fatal("watcher matched an unrelated file")
	}
}

func TestWatcherReloadsAndCancels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloads atomic.Int64
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWatcher(path, logger, func() error {
		reloads.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("watcher returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop on cancellation")
		}
	}()

	deadline := time.Now().Add(4 * time.Second)
	for reloads.Load() == 0 && time.Now().Before(deadline) {
		if err := os.WriteFile(path, []byte("server:\n  addr: \":8081\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
	}
	if reloads.Load() == 0 {
		t.Fatal("watcher did not trigger a reload")
	}

	// Unrelated files in the same directory must not trigger a reload.
	time.Sleep(400 * time.Millisecond)
	before := reloads.Load()
	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := reloads.Load(); got != before {
		t.Fatalf("unrelated file triggered %d reloads", got-before)
	}
}

func TestWatcherKeepsRunningAfterReloadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloads atomic.Int64
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWatcher(path, logger, func() error {
		reloads.Add(1)
		return errors.New("broken config")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop")
		}
	}()

	for target := int64(1); target <= 2; target++ {
		deadline := time.Now().Add(4 * time.Second)
		for reloads.Load() < target && time.Now().Before(deadline) {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("server: {}\n# changed %d\n", target)), 0o600); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		if reloads.Load() < target {
			t.Fatalf("watcher stopped calling reload after %d calls", reloads.Load())
		}
		time.Sleep(350 * time.Millisecond)
	}
}
