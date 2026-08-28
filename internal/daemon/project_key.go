package daemon

import (
	"context"

	"github.com/srjn45/warden/internal/projectkey"
)

// The project-key normalizer now lives in the leaf package internal/projectkey
// so every layer (daemon here, plus the TUI's project grouping) keys on one
// canonical identity — the normalizer cannot live in package daemon because the
// daemon imports the TUI (attach.go), which would cycle. These thin aliases keep
// the daemon's existing call sites and tests unchanged.

// LocalKeyPrefix tags project keys derived from a local path (design §4.1,
// decision 4). Re-exported from internal/projectkey.
const LocalKeyPrefix = projectkey.LocalKeyPrefix

// ProjectKey returns a stable identity key for a repository (remote key when
// available, else a `local:` path fallback). See projectkey.Key.
func ProjectKey(remoteURL, repoRoot string) string {
	return projectkey.Key(remoteURL, repoRoot)
}

// NormalizeRemoteURL canonicalizes a git remote URL into a "host/path" project
// key. See projectkey.NormalizeRemoteURL.
func NormalizeRemoteURL(raw string) (string, bool) {
	return projectkey.NormalizeRemoteURL(raw)
}

// ProjectKeyForDir resolves a directory's project key via its git remote (origin),
// with a `local:` repo-root fallback. See projectkey.ForDir.
func ProjectKeyForDir(ctx context.Context, dir string) string {
	return projectkey.ForDir(ctx, dir)
}
