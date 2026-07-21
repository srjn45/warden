package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadStrict_GoodBadMissing(t *testing.T) {
	// Good file decodes and validates.
	good := tmpConfig(t, "addr: 127.0.0.1:9999\nmodel_default: opus\n")
	cfg, err := LoadStrict(good)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9999", cfg.Addr)
	require.Equal(t, "opus", cfg.ModelDefault)

	// Malformed YAML is an error (NOT a silent fall back to defaults) so a reload
	// keeps the last-good config.
	bad := tmpConfig(t, "addr: [unterminated\n:::\n")
	_, err = LoadStrict(bad)
	require.Error(t, err)

	// A missing file is an error too (Load, by contrast, returns defaults).
	_, err = LoadStrict(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
	// Load stays lenient for the missing case (historic contract).
	require.Equal(t, defaults().Addr, Load(filepath.Join(t.TempDir(), "nope.yaml")).Addr)
}

// TestWatcherReloadOnceKeepsLastGood proves the core reload contract: a good edit
// fans out through onReload; a subsequent BAD edit routes to onError and does NOT
// call onReload — so the caller keeps whatever it last applied (last-good).
func TestWatcherReloadOnceKeepsLastGood(t *testing.T) {
	path := tmpConfig(t, "model_default: opus\n")

	var mu sync.Mutex
	var applied []string
	var errs int
	w, err := NewWatcher(path, 10*time.Millisecond,
		func(c Config) { mu.Lock(); applied = append(applied, c.ModelDefault); mu.Unlock() },
		func(error) { mu.Lock(); errs++; mu.Unlock() },
	)
	require.NoError(t, err)
	defer w.Close()

	// Good reload → onReload with the new value.
	w.reloadOnce()
	require.NoError(t, os.WriteFile(path, []byte("model_default: sonnet\n"), 0o644))
	w.reloadOnce()

	// Bad reload → onError, onReload untouched (last-good "sonnet" stands).
	require.NoError(t, os.WriteFile(path, []byte("model_default: [broken\n:::\n"), 0o644))
	w.reloadOnce()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"opus", "sonnet"}, applied, "only good edits reach onReload")
	require.Equal(t, 1, errs, "the bad edit routed to onError")
}

// TestWatcherDebounceCoalesces proves a burst of writes collapses into a single
// reload once the burst settles (editors emit several events per save).
func TestWatcherDebounceCoalesces(t *testing.T) {
	path := tmpConfig(t, "model_default: opus\n")

	var reloads int64
	fired := make(chan struct{}, 16)
	debounce := 120 * time.Millisecond
	w, err := NewWatcher(path, debounce,
		func(Config) { atomic.AddInt64(&reloads, 1); fired <- struct{}{} },
		nil,
	)
	require.NoError(t, err)
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// Burst: several rapid writes within one debounce window.
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(path, []byte("model_default: sonnet\n"), 0o644))
	}

	// The burst must produce exactly one reload.
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a debounced reload after the burst")
	}
	// No second reload should follow from the same burst.
	select {
	case <-fired:
		t.Fatal("burst was not coalesced — got a second reload")
	case <-time.After(3 * debounce):
	}
	require.Equal(t, int64(1), atomic.LoadInt64(&reloads))
}
