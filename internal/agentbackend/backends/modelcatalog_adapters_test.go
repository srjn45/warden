package backends

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func withCatalogRunner(t *testing.T, f func(context.Context, string, ...string) ([]byte, error)) {
	t.Helper()
	old := modelCatalogRun
	modelCatalogRun = f
	t.Cleanup(func() { modelCatalogRun = old })
}

func TestSlowCatalogProviderDoesNotBlockCatalogStoreReaders(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	withCatalogRunner(t, func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		close(entered)
		<-release
		return []byte("Available Models\ngpt-test - Test\n"), nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = (Cursor{}).DiscoverModels(context.Background(), agentbackend.CatalogScope{})
	}()
	<-entered

	// Discovery is adapter I/O and is intentionally outside backendstore.Store.mu.
	// This is deterministic: the provider remains blocked until after the reader
	// returns, proving catalog reads/sessions need not wait for it.
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	read := make(chan struct{})
	go func() { _, _ = s.ListModels(backendstore.ModelTier("")); close(read) }()
	select {
	case <-read:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("catalog store reader blocked behind slow provider")
	}
	close(release)
	<-done
}

func TestCodexCatalogUsesCodexHomeCacheNotConfigDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{
 "fetched_at":"2026-10-19T17:08:00Z", "client_version":"0.160.1", "identity":"account-a",
 "models":[{"slug":"gpt-6-sol","display_name":"GPT 6 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]},{"slug":"gpt-reserve","visibility":"hide"}]
}`), 0600))
	oldHome := codexHome
	codexHome = func() string { return dir }
	t.Cleanup(func() { codexHome = oldHome })
	withCatalogRunner(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		require.Equal(t, "codex", name)
		require.Equal(t, []string{"--version"}, args)
		return []byte("0.160.1\n"), nil
	})

	s, err := (Codex{}).ResolveScope(context.Background())
	require.NoError(t, err)
	require.Equal(t, "account-a", s.AccountRef)
	o := (Codex{}).DiscoverModels(context.Background(), s)
	require.Equal(t, agentbackend.CatalogComplete, o.Completeness)
	require.Len(t, o.Models, 2)
	require.Equal(t, "hidden", o.Models[1].Visibility)
}

func TestCodexCatalogStaleOrIdentityMismatchIsNeverComplete(t *testing.T) {
	dir := t.TempDir()
	cache := `{"fetched_at":"2000-01-01T00:00:00Z","client_version":"0.160.1","identity":"cache-account","models":[{"slug":"x"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(cache), 0600))
	o := (Codex{}).DiscoverModels(context.Background(), agentbackend.CatalogScope{AiCli: "codex", Profile: dir, AccountRef: "other", CLIVersion: "0.160.1"})
	require.Equal(t, agentbackend.CatalogPartial, o.Completeness)
	require.Equal(t, "identity_mismatch", o.Err.Class)
	o = (Codex{}).DiscoverModels(context.Background(), agentbackend.CatalogScope{AiCli: "codex", Profile: dir, AccountRef: "cache-account", CLIVersion: "0.160.1"})
	require.Equal(t, agentbackend.CatalogPartial, o.Completeness)
	require.Equal(t, "stale_cache", o.Err.Class)
}

func TestAntigravityCatalogTabRowsAreExactIDsAndLegacyIsPartial(t *testing.T) {
	withCatalogRunner(t, func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("gemini-high\tGemini High\nlegacy display\n"), nil
	})
	o := (Antigravity{}).DiscoverModels(context.Background(), agentbackend.CatalogScope{})
	require.Equal(t, agentbackend.CatalogPartial, o.Completeness)
	require.Equal(t, "gemini-high", o.Models[0].ModelID)
}

func TestCatalogOutputLimitIsPartialAndNoGenerationCommandIsAllowlisted(t *testing.T) {
	withCatalogRunner(t, func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("a - A\n"), errCatalogOutputLimit
	})
	o := (Cursor{}).DiscoverModels(context.Background(), agentbackend.CatalogScope{})
	require.Equal(t, agentbackend.CatalogPartial, o.Completeness)
	require.Equal(t, "partial_result", o.Err.Class)
	_, ok := catalogMetadataCommand("codex", []string{"exec", "hello"})
	require.False(t, ok)
}

func TestCrushWithoutConfiguredProvidersNeverReturnsPublicRegistry(t *testing.T) {
	old := modelCatalogUserConfigDir
	modelCatalogUserConfigDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { modelCatalogUserConfigDir = old })
	withCatalogRunner(t, func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, errors.New("must not run") })
	s, err := (Crush{}).ResolveScope(context.Background())
	require.NoError(t, err)
	o := (Crush{}).DiscoverModels(context.Background(), s)
	require.Equal(t, agentbackend.CatalogPartial, o.Completeness)
	require.Empty(t, o.Models)
	require.Equal(t, "scope_unresolved", o.Err.Class)
}
