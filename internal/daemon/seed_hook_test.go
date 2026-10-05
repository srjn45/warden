package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type bodyNotifier struct{ titles, bodies []string }

func (n *bodyNotifier) Notify(title, body string) {
	n.titles, n.bodies = append(n.titles, title), append(n.bodies, body)
}

func TestSeedOutcomeHookFailureAuditsAndNotifiesOnce(t *testing.T) {
	st, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "crush-1", Name: "crush-1", TmuxSession: "crush-1", Status: store.StatusWorking,
		SeedStatus: store.SeedPending, CreatedAt: now, UpdatedAt: now,
	}))
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	n := &bodyNotifier{}
	hook := NewSeedOutcomeHook(st, audit.NewWriter(auditPath), n)

	hook(lifecycle.SeedOutcome{AgentID: "crush-1", Backend: "crush", Status: store.SeedFailed,
		Err: "paste failed after 4 attempts", PromptFile: "/data/prompts/crush-1"})

	got, err := st.Get(context.Background(), "crush-1")
	require.NoError(t, err)
	require.Equal(t, store.SeedFailed, got.SeedStatus)
	require.Equal(t, "paste failed after 4 attempts", got.SeedError)

	evs, err := audit.Read(auditPath, audit.Filter{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, audit.ActionPromptSeedFailed, evs[0].Action)
	require.Equal(t, "crush-1", evs[0].Target)
	require.Equal(t, "/data/prompts/crush-1", evs[0].Detail["prompt_file"])

	require.Len(t, n.bodies, 1, "exactly one operator notification")
	require.Contains(t, n.bodies[0], "crush-1")
	require.Contains(t, n.bodies[0], "/data/prompts/crush-1")
}

func TestSeedOutcomeHookDeliveredIsQuiet(t *testing.T) {
	st, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "crush-1", Name: "crush-1", TmuxSession: "crush-1", Status: store.StatusWorking,
		SeedStatus: store.SeedFailed, SeedError: "old", CreatedAt: now, UpdatedAt: now,
	}))
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	n := &bodyNotifier{}
	NewSeedOutcomeHook(st, audit.NewWriter(auditPath), n)(lifecycle.SeedOutcome{AgentID: "crush-1", Status: store.SeedDelivered})

	got, _ := st.Get(context.Background(), "crush-1")
	require.Equal(t, store.SeedDelivered, got.SeedStatus)
	require.Empty(t, got.SeedError)
	require.Empty(t, n.bodies)
	evs, _ := audit.Read(auditPath, audit.Filter{})
	require.Empty(t, evs)
}
