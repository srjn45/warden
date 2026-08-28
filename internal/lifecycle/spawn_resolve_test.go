package lifecycle

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// stubResolveCapture records the last ResolveOptions it saw and returns a fixed
// resolution (or error), so a test can assert both what the router was asked and
// that it was consulted only on the unpinned path. It satisfies SuccessorResolver;
// resolveSpawnTarget always goes through Resolve (never ResolveTier).
type stubResolveCapture struct {
	res    *router.Resolution
	err    error
	called bool
	opts   router.ResolveOptions
}

func (c *stubResolveCapture) ResolveTier(_ context.Context, _ backendstore.ModelTier) (*router.Resolution, error) {
	return c.res, c.err
}

func (c *stubResolveCapture) Resolve(_ context.Context, opts router.ResolveOptions) (*router.Resolution, error) {
	c.called = true
	c.opts = opts
	return c.res, c.err
}

func TestResolveSpawnTarget(t *testing.T) {
	pick := &router.Resolution{BackendID: "codex", ModelID: "o1"}

	t.Run("pinned backend and model win, router untouched", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "antigravity", "gemini")
		require.Equal(t, "antigravity", b)
		require.Equal(t, "gemini", m)
		require.False(t, cr.called, "resolver must not be consulted when a backend is pinned")
	})

	t.Run("pinned backend, empty model keeps backend default, router untouched", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m := lc.resolveSpawnTarget(context.Background(), "", "", "", "aider", "")
		require.Equal(t, "aider", b)
		require.Equal(t, "", m)
		require.False(t, cr.called)
	})

	t.Run("role-default model wins over the router", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		// backend empty (default), model set by a role default: keep it, stay on default backend.
		b, m := lc.resolveSpawnTarget(context.Background(), "worker", "", "", "", "sonnet")
		require.Equal(t, "", b, "backend stays the config default when only a model is pinned")
		require.Equal(t, "sonnet", m)
		require.False(t, cr.called, "resolver must not override a pinned/role-default model")
	})

	t.Run("router picks backend+model when nothing is pinned", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "", "")
		require.Equal(t, "codex", b)
		require.Equal(t, "o1", m)
		require.True(t, cr.called)
		require.Equal(t, "worker", cr.opts.Role)
		require.Equal(t, "development", cr.opts.Task)
		require.EqualValues(t, "tier-1", cr.opts.Tier)
		require.True(t, cr.opts.AllowFallback, "spawn must allow tier fallback so a first spawn always finds a model")
	})

	t.Run("nil resolver degrades to request defaults", func(t *testing.T) {
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = nil
		b, m := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "", "")
		require.Equal(t, "", b)
		require.Equal(t, "", m)
	})

	t.Run("resolver error degrades to request defaults (spawn never hard-fails)", func(t *testing.T) {
		cr := &stubResolveCapture{err: router.ErrNoCandidate}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m := lc.resolveSpawnTarget(context.Background(), "worker", "", "tier-1", "", "")
		require.Equal(t, "", b)
		require.Equal(t, "", m)
		require.True(t, cr.called)
	})

	t.Run("resolver empty resolution degrades to request defaults", func(t *testing.T) {
		// ErrAllExhausted returns a non-nil Resolution with an empty BackendID.
		cr := &stubResolveCapture{res: &router.Resolution{}, err: router.ErrAllExhausted}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m := lc.resolveSpawnTarget(context.Background(), "", "", "tier-2", "", "")
		require.Equal(t, "", b)
		require.Equal(t, "", m)
	})
}
