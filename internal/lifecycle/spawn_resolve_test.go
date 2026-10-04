package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// stubResolveCapture records the last ResolveOptions it saw and returns a fixed
// resolution (or error), so a test can assert both what the router was asked and
// that it was consulted only on the unpinned / aicli-without-model paths. It
// satisfies SuccessorResolver; resolveSpawnTarget always goes through Resolve
// (never ResolveTier).
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
	claudePick := &router.Resolution{BackendID: "claude", ModelID: "claude-sonnet-4-6"}

	t.Run("exact pin: both aicli and model win, router untouched", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m, err := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "antigravity", "gemini")
		require.NoError(t, err)
		require.Equal(t, "antigravity", b)
		require.Equal(t, "gemini", m)
		require.False(t, cr.called, "resolver must not be consulted on an exact pin")
	})

	t.Run("aicli pin without model: PreferredBackend resolves optimal model", func(t *testing.T) {
		cr := &stubResolveCapture{res: claudePick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m, err := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "claude", "")
		require.NoError(t, err)
		require.Equal(t, "claude", b)
		require.Equal(t, "claude-sonnet-4-6", m)
		require.True(t, cr.called)
		require.Equal(t, "claude", cr.opts.PreferredBackend)
		require.Equal(t, "worker", cr.opts.Role)
		require.Equal(t, "development", cr.opts.Task)
		require.EqualValues(t, "tier-1", cr.opts.Tier)
		require.True(t, cr.opts.AllowFallback)
		require.NotEmpty(t, m, "model must never be left empty")
	})

	t.Run("model without aicli is a validation error", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		_, _, err := lc.resolveSpawnTarget(context.Background(), "worker", "", "", "", "sonnet")
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrModelRequiresAiCli))
		require.False(t, cr.called, "resolver must not run when config is invalid")
	})

	t.Run("router picks aicli+model when nothing is pinned", func(t *testing.T) {
		cr := &stubResolveCapture{res: pick}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m, err := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "", "")
		require.NoError(t, err)
		require.Equal(t, "codex", b)
		require.Equal(t, "o1", m)
		require.True(t, cr.called)
		require.Equal(t, "worker", cr.opts.Role)
		require.Equal(t, "development", cr.opts.Task)
		require.EqualValues(t, "tier-1", cr.opts.Tier)
		require.Empty(t, cr.opts.PreferredBackend)
		require.True(t, cr.opts.AllowFallback, "spawn must allow tier fallback so a first spawn always finds a model")
	})

	t.Run("nil resolver degrades to default model (never empty)", func(t *testing.T) {
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = nil
		b, m, err := lc.resolveSpawnTarget(context.Background(), "worker", "development", "tier-1", "", "")
		require.NoError(t, err)
		require.Equal(t, "", b)
		require.Equal(t, DefaultModel, m)
	})

	t.Run("aicli pin with nil resolver keeps aicli and fills default model", func(t *testing.T) {
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = nil
		b, m, err := lc.resolveSpawnTarget(context.Background(), "", "", "", "codex", "")
		require.NoError(t, err)
		require.Equal(t, "codex", b)
		require.Equal(t, DefaultModel, m)
	})

	t.Run("resolver error degrades to default model (spawn never hard-fails)", func(t *testing.T) {
		cr := &stubResolveCapture{err: router.ErrNoCandidate}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m, err := lc.resolveSpawnTarget(context.Background(), "worker", "", "tier-1", "", "")
		require.NoError(t, err)
		require.Equal(t, "", b)
		require.Equal(t, DefaultModel, m)
		require.True(t, cr.called)
	})

	t.Run("resolver empty resolution degrades to default model", func(t *testing.T) {
		// ErrAllExhausted returns a non-nil Resolution with an empty BackendID.
		cr := &stubResolveCapture{res: &router.Resolution{}, err: router.ErrAllExhausted}
		lc := New(&FakeRunner{}, &FakeConfig{})
		lc.Resolver = cr
		b, m, err := lc.resolveSpawnTarget(context.Background(), "", "", "tier-2", "", "")
		require.NoError(t, err)
		require.Equal(t, "", b)
		require.Equal(t, DefaultModel, m)
	})

	t.Run("unknown aicli on exact pin is rejected", func(t *testing.T) {
		lc := New(&FakeRunner{}, &FakeConfig{})
		_, _, err := lc.resolveSpawnTarget(context.Background(), "", "", "", "not-a-real-cli", "some-model")
		require.Error(t, err)
	})
}
