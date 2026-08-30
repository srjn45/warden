package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// TestRenderPeerContext locks the pure rendering: a blank group projects nothing; a
// grouped-but-solo orchestrator still learns its group; peers are listed with the
// send_message capability spelled out.
func TestRenderPeerContext(t *testing.T) {
	require.Equal(t, "", renderPeerContext("", []string{"orch-a"}), "no group name projects nothing")
	require.Equal(t, "", renderPeerContext("  ", nil), "blank group name projects nothing")

	solo := renderPeerContext("Payments", nil)
	require.Contains(t, solo, `Project Group "Payments"`, "solo orch still learns its group")
	require.Contains(t, solo, "only live orchestrator", "solo orch is told it is alone")
	require.NotContains(t, solo, "send_message", "no peers ⇒ no coordination line")

	withPeers := renderPeerContext("Payments", []string{"orch-billing", "orch-ledger"})
	require.Contains(t, withPeers, `Project Group "Payments"`)
	require.Contains(t, withPeers, "orch-billing, orch-ledger", "peers are listed in order")
	require.Contains(t, withPeers, "send_message", "the coordination capability is named")
	require.Contains(t, withPeers, "mcp__warden__send_message")
}

// peerTestServer builds a Server with a fake session store and a real (temp)
// project store, the minimum PeerContext needs.
func peerTestServer(t *testing.T) (*Server, *fakeStore, *projectstore.Store) {
	t.Helper()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	fs := newFakeStore()
	return &Server{store: fs, life: &fakeLife{}, projects: ps}, fs, ps
}

func TestPeerContextSkipsNonOrchestrator(t *testing.T) {
	srv, _, _ := peerTestServer(t)
	// A worker, even one pinned to a grouped project, gets no peer context.
	got := srv.PeerContext(context.Background(), &store.Session{
		ID: "w1", Role: "worker", ProjectID: "p1", Status: store.StatusWorking,
	})
	require.Equal(t, "", got)
}

func TestPeerContextSkipsWithoutProjectOrGroup(t *testing.T) {
	srv, _, ps := peerTestServer(t)
	// An orchestrator with no project back-ref: nothing to resolve.
	require.Equal(t, "", srv.PeerContext(context.Background(), &store.Session{
		ID: "o1", Role: orchestratorRole, Status: store.StatusWorking,
	}))
	// An orchestrator whose project belongs to no group.
	require.Equal(t, "", srv.PeerContext(context.Background(), &store.Session{
		ID: "o2", Role: orchestratorRole, ProjectID: "lonely", Status: store.StatusWorking,
	}))
	// Even with an unrelated group present, a non-member project stays contextless.
	_, err := ps.CreateGroup(projectstore.ProjectGroup{Name: "Other", ProjectIDs: []string{"pX"}})
	require.NoError(t, err)
	require.Equal(t, "", srv.PeerContext(context.Background(), &store.Session{
		ID: "o2", Role: orchestratorRole, ProjectID: "lonely", Status: store.StatusWorking,
	}))
}

func TestPeerContextListsLivePeersExcludingSelfAndDead(t *testing.T) {
	srv, fs, ps := peerTestServer(t)
	ctx := context.Background()
	// One group spanning three projects.
	_, err := ps.CreateGroup(projectstore.ProjectGroup{
		Name: "Platform", ProjectIDs: []string{"proj-a", "proj-b", "proj-c"},
	})
	require.NoError(t, err)

	self := &store.Session{ID: "self", Name: "orch-a", Role: orchestratorRole, ProjectID: "proj-a", Status: store.StatusWorking}
	fs.Insert(ctx, self)
	// A live peer orch in a sibling project — included.
	fs.Insert(ctx, &store.Session{ID: "pb", Name: "orch-b", Role: orchestratorRole, ProjectID: "proj-b", Status: store.StatusIdle})
	// A dead peer orch — excluded (not live).
	fs.Insert(ctx, &store.Session{ID: "pc", Name: "orch-c", Role: orchestratorRole, ProjectID: "proj-c", Status: store.StatusDone})
	// A live worker in a member project — excluded (not an orchestrator).
	fs.Insert(ctx, &store.Session{ID: "w", Name: "worker-1", Role: "worker", ProjectID: "proj-b", Status: store.StatusWorking})
	// A live orch OUTSIDE the group — excluded (not a member project).
	fs.Insert(ctx, &store.Session{ID: "out", Name: "orch-z", Role: orchestratorRole, ProjectID: "proj-z", Status: store.StatusWorking})

	got := srv.PeerContext(ctx, self)
	require.Contains(t, got, `Project Group "Platform"`)
	require.Contains(t, got, "orch-b", "the live sibling orch is a peer")
	require.NotContains(t, got, "orch-a", "self is excluded")
	require.NotContains(t, got, "orch-c", "a dead orch is not a peer")
	require.NotContains(t, got, "worker-1", "a non-orchestrator is not a peer")
	require.NotContains(t, got, "orch-z", "an out-of-group orch is not a peer")

	// Direct check of the peer collector: sorted, self-excluded.
	peers := srv.livePeerOrchestrators(ctx, mustGroup(t, ps, "Platform"), self)
	require.Equal(t, []string{"orch-b"}, peers)
}

func TestPeerContextFirstGroupWinsOnOverlap(t *testing.T) {
	srv, fs, ps := peerTestServer(t)
	ctx := context.Background()
	// proj-a is (mis)configured into two groups; ListGroups is name-sorted, so the
	// alphabetically-first group ("Alpha") wins deterministically.
	_, err := ps.CreateGroup(projectstore.ProjectGroup{Name: "Beta", ProjectIDs: []string{"proj-a"}})
	require.NoError(t, err)
	_, err = ps.CreateGroup(projectstore.ProjectGroup{Name: "Alpha", ProjectIDs: []string{"proj-a"}})
	require.NoError(t, err)

	self := &store.Session{ID: "self", Name: "orch-a", Role: orchestratorRole, ProjectID: "proj-a", Status: store.StatusWorking}
	fs.Insert(ctx, self)
	got := srv.PeerContext(ctx, self)
	require.Contains(t, got, `Project Group "Alpha"`, "first group (name-sorted) wins on overlap")
	require.NotContains(t, got, "Beta")
}

func mustGroup(t *testing.T, ps *projectstore.Store, name string) projectstore.ProjectGroup {
	t.Helper()
	groups, err := ps.ListGroups()
	require.NoError(t, err)
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %q not found", name)
	return projectstore.ProjectGroup{}
}
