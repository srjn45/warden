package config

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultReloadDebounce coalesces a burst of filesystem events (an editor often
// emits several — rename, chmod, write — for a single save) into one reload.
const DefaultReloadDebounce = 500 * time.Millisecond

// Watcher live-reloads the config file: it watches for changes and, after a
// debounce, re-reads the file and fans the new config out through onReload. A
// bad edit (unparseable/undecodable) never reaches onReload — onError is called
// instead and the caller keeps whatever config it last applied (last-good).
//
// It watches the config file's PARENT DIRECTORY, not the file inode, because
// editors (and warden's own atomic writes, see writeFile) replace the file via
// rename: the original inode is unlinked, so an inode-level watch goes deaf
// after the first save. Watching the directory and filtering by filename catches
// every rename-in-place. Events for sibling files in the directory are ignored.
type Watcher struct {
	path     string // absolute config path we react to
	dir      string // parent directory actually registered with fsnotify
	base     string // config filename, used to filter directory events
	debounce time.Duration
	fsw      *fsnotify.Watcher

	// onReload receives the freshly loaded, validated config after a successful
	// reload. onError receives the load error after a failed reload; the config
	// is NOT changed in that case (last-good is kept by the caller).
	onReload func(Config)
	onError  func(error)
}

// NewWatcher constructs a config Watcher over path. debounce<=0 uses
// DefaultReloadDebounce. onReload is required; onError may be nil (a failed
// reload is then logged only). The returned Watcher owns an fsnotify handle and
// must be run with Run and released with Close.
func NewWatcher(path string, debounce time.Duration, onReload func(Config), onError func(error)) (*Watcher, error) {
	if debounce <= 0 {
		debounce = DefaultReloadDebounce
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return nil, err
	}
	return &Watcher{
		path:     abs,
		dir:      dir,
		base:     filepath.Base(abs),
		debounce: debounce,
		fsw:      fsw,
		onReload: onReload,
		onError:  onError,
	}, nil
}

// Run drives the watch loop until ctx is cancelled, then returns ctx.Err(). It
// coalesces bursts: a debounce timer is (re)armed on each matching event and one
// reload fires once the burst settles. Safe to call once per Watcher.
func (w *Watcher) Run(ctx context.Context) error {
	// A stopped timer with a drained channel; armed on the first matching event.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil // watcher closed
			}
			if !w.matches(ev.Name) {
				continue // sibling file in the same directory — ignore
			}
			// (Re)arm the debounce: drain a pending fire first so Reset is clean.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.debounce)
		case <-timer.C:
			w.reloadOnce()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("config watcher: fsnotify error", "err", err)
		}
	}
}

// matches reports whether a directory event names the watched config file.
// Compared by base name so an atomic-rename replacement (temp → config.yaml) is
// caught regardless of the event's reported path form.
func (w *Watcher) matches(name string) bool {
	return filepath.Base(name) == w.base
}

// reloadOnce performs a single reload cycle: re-read + re-validate, then fan out
// the result. A load error keeps the last-good config (onReload is skipped) and
// routes to onError. Exposed to package tests so the reload/keep-last-good
// contract can be exercised without driving real filesystem events.
func (w *Watcher) reloadOnce() {
	cfg, err := LoadStrict(w.path)
	if err != nil {
		slog.Warn("config: reload failed — keeping last-good config", "path", w.path, "err", err)
		if w.onError != nil {
			w.onError(err)
		}
		return
	}
	slog.Info("config: reloaded — applying live", "path", w.path)
	w.onReload(cfg)
}

// Close stops watching and releases the fsnotify handle.
func (w *Watcher) Close() error { return w.fsw.Close() }
