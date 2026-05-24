package config

import (
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// OnReloadFunc is called when configuration is successfully reloaded.
// The callback receives the new config; the caller is responsible for
// rebuilding providers, updating registry, etc.
type OnReloadFunc func(cfg *Config)

// Watcher monitors a config file and triggers hot reload.
type Watcher struct {
	path     string
	debounce time.Duration
	onReload OnReloadFunc

	// Current state for comparison
	mu         sync.Mutex
	currentCfg *Config

	watcher *fsnotify.Watcher
	done    chan struct{}
}

// NewWatcher creates a new config file watcher.
func NewWatcher(path string, currentCfg *Config, onReload OnReloadFunc) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Watch the directory (editors may delete+recreate the file)
	dir := filepath.Dir(path)
	if err := fsw.Add(dir); err != nil {
		fsw.Close()
		return nil, err
	}

	w := &Watcher{
		path:       path,
		debounce:   300 * time.Millisecond,
		onReload:   onReload,
		currentCfg: currentCfg,
		watcher:    fsw,
		done:       make(chan struct{}),
	}

	go w.loop()
	return w, nil
}

func (w *Watcher) loop() {
	defer close(w.done)

	base := filepath.Base(w.path)
	var timer *time.Timer

	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Only react to events on our config file
			if filepath.Base(event.Name) != base {
				continue
			}

			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				if event.Op&fsnotify.Remove != 0 {
					slog.Warn("config file removed, keeping current config", "path", w.path)
				}
				continue
			}

			// Debounce: reset timer on each event
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(w.debounce, w.reload)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("config watcher error", "error", err)
		}
	}
}

func (w *Watcher) reload() {
	slog.Info("config file changed, reloading", "path", w.path)

	newCfg, err := Load(w.path)
	if err != nil {
		slog.Error("config reload failed", "error", err)
		return
	}

	w.mu.Lock()
	oldCfg := w.currentCfg
	w.currentCfg = newCfg
	w.mu.Unlock()

	// Check for non-hot-reloadable changes
	w.checkImmutableFields(oldCfg, newCfg)

	slog.Info("config reloaded successfully",
		"providers", len(newCfg.Providers),
		"api_keys", len(newCfg.VirtualAPIKeys),
	)

	w.onReload(newCfg)
}

func (w *Watcher) checkImmutableFields(oldCfg, newCfg *Config) {
	if oldCfg.Host != newCfg.Host {
		slog.Warn("config: host change ignored, restart required",
			"old", oldCfg.Host, "new", newCfg.Host)
	}
	if oldCfg.Port != newCfg.Port {
		slog.Warn("config: port change ignored, restart required",
			"old", oldCfg.Port, "new", newCfg.Port)
	}
	if oldCfg.Database.DSN != newCfg.Database.DSN {
		slog.Warn("config: database path change ignored, restart required",
			"old", oldCfg.Database.DSN, "new", newCfg.Database.DSN)
	}
}

// Close stops the watcher.
func (w *Watcher) Close() {
	w.watcher.Close()
	<-w.done
}
