package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestAdapterInteractiveSpawnStaysFreeForm guards the wire-DTO → lifecycle
// translation for an empty-prompt (interactive) spawn. The adapter must NOT
// normalize an empty Type: NormalizeType("") collapses to "other", which would
// flip lifecycle.Spawn off its free-form cwd-launch path onto the typed/managed
// path (requiring a repo + worktree) and break interactive spawn. This path is
// not covered by the route tests (they use fakeLife) or the lifecycle tests
// (they call Spawn directly), so the adapter glue is exercised here.
func TestAdapterInteractiveSpawnStaysFreeForm(t *testing.T) {
	lc := lifecycle.New(&lifecycle.FakeRunner{})
	a := NewLifecycleAdapter(lc, newFakeStore())

	sess, err := a.Spawn(context.Background(), SpawnRequest{Cwd: t.TempDir()})
	require.NoError(t, err, "empty-prompt free-form spawn must launch in cwd, not require a repo")
	require.Equal(t, store.Type(""), sess.Type, "interactive spawn stays untyped (classifying), not 'other'")
	require.Equal(t, "interactive", sess.Subject)
}

// TestAdapterTypedSpawnNormalizes confirms the typed path still normalizes an
// unknown type down to "other".
func TestAdapterTypedSpawnNormalizes(t *testing.T) {
	lc := lifecycle.New(&lifecycle.FakeRunner{})
	a := NewLifecycleAdapter(lc, newFakeStore())

	sess, err := a.Spawn(context.Background(), SpawnRequest{Type: "bogus", Repo: t.TempDir()})
	require.NoError(t, err)
	require.Equal(t, store.TypeOther, sess.Type, "unknown type collapses to other")
}
