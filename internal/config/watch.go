package config

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher triggers a reload when the config file changes. It watches the
// parent directory rather than the file so Kubernetes ConfigMap symlink swaps
// and editors that replace files are detected.
type Watcher struct {
	path     string
	dir      string
	base     string
	logger   *slog.Logger
	reload   func() error
	debounce time.Duration
}

// NewWatcher creates a watcher for path. reload is called after changes are
// debounced; errors are logged and leave the previous config serving.
func NewWatcher(path string, logger *slog.Logger, reload func() error) (*Watcher, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("config path is empty")
	}
	if reload == nil {
		return nil, errors.New("reload callback is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &Watcher{
		path:     abs,
		dir:      filepath.Dir(abs),
		base:     filepath.Base(abs),
		logger:   logger,
		reload:   reload,
		debounce: 250 * time.Millisecond,
	}, nil
}

// Run blocks until ctx is cancelled or the watcher fails. A nil return means a
// clean shutdown.
func (w *Watcher) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	if err := watcher.Add(w.dir); err != nil {
		return err
	}
	w.logger.Info("watching config for changes", "path", w.path)

	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !w.matches(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}
			timerC = timer.C
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("config watcher error", "error", err)
		case <-timerC:
			timerC = nil
			if err := w.reload(); err != nil {
				w.logger.Error("config reload failed; keeping previous config", "error", err)
			}
		}
	}
}

func (w *Watcher) matches(name string) bool {
	return strings.EqualFold(filepath.Base(name), w.base)
}
