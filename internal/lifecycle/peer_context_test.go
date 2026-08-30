package lifecycle

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestPeerGuidanceSeam locks the store-free seam: with no provider wired the addendum
// is empty; with one wired, its output is returned verbatim and the session is passed
// through so the daemon closure can key off role/project.
func TestPeerGuidanceSeam(t *testing.T) {
	ctx := context.Background()

	off := New(&FakeRunner{}, &FakeConfig{})
	require.Equal(t, "", off.peerGuidance(ctx, &store.Session{ID: "x"}), "no provider ⇒ nothing")
	require.Equal(t, "", off.peerGuidance(ctx, nil), "nil session ⇒ nothing, never a panic")

	on := New(&FakeRunner{}, &FakeConfig{})
	var seen *store.Session
	on.PeerContextFn = func(_ context.Context, sess *store.Session) string {
		seen = sess
		if sess.Role == "orchestrator" {
			return "PEER-MARKER"
		}
		return ""
	}
	require.Equal(t, "PEER-MARKER", on.peerGuidance(ctx, &store.Session{ID: "o1", Role: "orchestrator"}))
	require.Equal(t, "o1", seen.ID, "the session is threaded to the provider")
	require.Equal(t, "", on.peerGuidance(ctx, &store.Session{ID: "g1"}), "provider gates non-orchestrators")
}

// TestSpawnProjectsPeerContextIntoLaunch proves the win end-to-end: a wired provider
// rides Claude's --append-system-prompt seam into a free-form orchestrator launch.
func TestSpawnProjectsPeerContextIntoLaunch(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr, &FakeConfig{})
	lc.PeerContextFn = func(context.Context, *store.Session) string {
		return "PEER-AWARENESS-BLOCK naming your Project Group and peers"
	}
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Cwd: t.TempDir(), Name: "orch-demo", Role: "orchestrator",
	})
	require.NoError(t, err)

	launch := findLaunch(t, fr, s.ID)
	require.Contains(t, launch, "PEER-AWARENESS-BLOCK", "peer context rides the launch")
	require.Contains(t, launch, "--append-system-prompt", "peer context rides Claude's flag seam")
}

// TestSpawnPeerContextByteIdenticalWhenNoProvider is the regression-lock: with no
// PeerContextFn wired, an orchestrator launch carries no peer fragment, so the launch
// is exactly what it was before Phase 3.
func TestSpawnPeerContextByteIdenticalWhenNoProvider(t *testing.T) {
	spawn := func(fn func(context.Context, *store.Session) string) string {
		fr := &FakeRunner{}
		lc := New(fr, &FakeConfig{})
		lc.PeerContextFn = fn
		s, err := lc.Spawn(context.Background(), SpawnRequest{
			Cwd: "/master", Ticket: "orch-demo", Role: "orchestrator",
		})
		require.NoError(t, err)
		return normSessionID(findLaunch(t, fr, s.ID))
	}
	baseline := spawn(nil)
	// A provider that returns "" (e.g. an ungrouped orch) must also be byte-identical.
	empty := spawn(func(context.Context, *store.Session) string { return "" })
	require.Equal(t, baseline, empty, "an empty peer context must not perturb the launch")
}
