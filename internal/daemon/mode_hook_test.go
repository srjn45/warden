package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func TestModeNormalizedHookPersistsAndAudits(t *testing.T) {
	ctx := context.Background()
	st, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	require.NoError(t, st.Insert(ctx, &agentstore.Agent{
		ID: "a1", Name: "a1", TmuxSession: "a1", AiCli: "cursor", PermissionMode: "acceptEdits",
		Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
	}))
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	hook := NewModeNormalizedHook(st, audit.NewWriter(auditPath))

	hook(ctx, lifecycle.ModeNormalization{
		AgentID: "a1", Path: "restore", From: "acceptEdits", To: "default", Persist: true,
		Rationale: lifecycle.RelaunchRationale{Outcome: lifecycle.OutcomeSteppedDown, Reasons: []string{"stepped down"}},
	})

	got, err := st.Get(ctx, "a1")
	require.NoError(t, err)
	require.Equal(t, "default", got.PermissionMode)
	var found bool
	for _, e := range got.Events {
		if e.Type == EventRelaunchPermissionMode {
			found = true
		}
	}
	require.True(t, found, "agent event recorded")
	raw, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), audit.ActionRelaunchModeNormalized)
}
