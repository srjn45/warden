package agentstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCRUD(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	a := &Agent{ID: "agent-1", Name: "worker", Status: store.StatusWorking, PipelineID: "p1", JobID: "build", Tags: []string{"autopilot"}, ParentID: "parent", ChildAgents: []string{"child"}}
	require.NoError(t, s.Insert(ctx, a))
	got, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.PipelineID, got.PipelineID)
	require.Equal(t, a.ChildAgents, got.ChildAgents)
	require.NoError(t, s.Update(ctx, a.ID, func(a *Agent) error { a.Status = store.StatusIdle; return nil }))
	got, err = s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, got.Status)
	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, s.Delete(ctx, a.ID))
	_, err = s.Get(ctx, a.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSpawnCreatesAndInitializesAgent(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	a := &Agent{ID: "agent-spawn", Name: "worker", Status: store.StatusSpawning}
	require.NoError(t, s.Spawn(context.Background(), a, "tmux-agent-spawn", "ai-session-1"))
	got, err := s.Get(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "tmux-agent-spawn", got.TmuxSession)
	require.Equal(t, "ai-session-1", got.AICLISessionID)
}

func TestCreateCanonicalizesStatusAliases(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Create(context.Background(), &Agent{ID: "busy-agent", Status: store.Status("busy")}))
	got, err := s.Get(context.Background(), "busy-agent")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status)
}

func TestTerminateAndDelete(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	require.NoError(t, s.Spawn(ctx, &Agent{ID: "agent-stop", Status: store.StatusWorking}, "tmux-stop", "ai-stop"))
	require.NoError(t, s.Terminate(ctx, "agent-stop"))
	got, err := s.Get(ctx, "agent-stop")
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, got.Status)
	require.NoError(t, s.Delete(ctx, "agent-stop"))
	_, err = s.Get(ctx, "agent-stop")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRecoverOnlyRevivesOrphanedAgent(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	require.NoError(t, s.Insert(ctx, &Agent{ID: "orphan", Status: store.StatusOrphaned}))
	require.NoError(t, s.Recover(ctx, "orphan"))
	got, err := s.Get(ctx, "orphan")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status)

	require.NoError(t, s.Insert(ctx, &Agent{ID: "done", Status: store.StatusDone}))
	require.ErrorIs(t, s.Recover(ctx, "done"), ErrNotOrphaned)
}

func TestMigratesOnlyActiveNonTerminalSessions(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-live", Status: store.StatusWorking, AutopilotRunID: "ap", PipelineID: "p", JobID: "j", ChildPipelines: []string{"child"}}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "terminal-live", Kind: store.KindTerminal, Status: store.StatusIdle}))
	require.NoError(t, legacy.Archive(ctx, "agent-live"))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-current", Status: store.StatusWorking, Tags: []string{"tag"}}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	list, err := agents.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "agent-current", list[0].ID)
	require.Equal(t, []string{"tag"}, list[0].Tags)
}

func TestMigrationImportsExistingActiveAgentFields(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	want := &store.Session{ID: "agent-live", Status: store.StatusWorking, PipelineID: "pipe", JobID: "job", Tags: []string{"autopilot"}, ParentID: "parent", ChildAgents: []string{"child"}, ChildPipelines: []string{"pipeline"}, AutopilotRunID: "run", AutopilotSlot: store.AutopilotSlotWorker, ProjectID: "project"}
	require.NoError(t, legacy.Insert(ctx, want))
	require.NoError(t, legacy.Close(ctx))
	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	got, err := agents.Get(ctx, want.ID)
	require.NoError(t, err)
	require.Equal(t, want.PipelineID, got.PipelineID)
	require.Equal(t, want.JobID, got.JobID)
	require.Equal(t, want.Tags, got.Tags)
	require.Equal(t, want.ChildAgents, got.ChildAgents)
	require.Equal(t, want.ChildPipelines, got.ChildPipelines)
	require.Equal(t, want.AutopilotRunID, got.AutopilotRunID)
}

func TestMigrationMarkerPreventsDuplicateImport(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(context.Background(), &store.Session{ID: "agent-1"}))
	require.NoError(t, legacy.Close(context.Background()))
	s, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// Reopen verifies the completed migration rather than trying to insert rows again.
	s, err = New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

// TestLegacyClaudeSessionIDDecode verifies that old persisted records that carry
// the deprecated "claude_session_id" field are decoded into the canonical
// AICLISessionID field on the Agent struct.
func TestLegacyClaudeSessionIDDecode(t *testing.T) {
	// Craft a raw JSON payload using only the old field names (as if written by
	// a pre-rename daemon) and verify it round-trips to the canonical fields.
	raw := `{
		"id":               "agent-legacy",
		"backend":          "claude",
		"claude_session_id":"old-uuid-1234",
		"status":           "working"
	}`
	var a Agent
	require.NoError(t, json.Unmarshal([]byte(raw), &a))
	require.Equal(t, "claude", a.AiCli, "legacy 'backend' should populate AiCli")
	require.Equal(t, "old-uuid-1234", a.AICLISessionID, "legacy 'claude_session_id' should populate AICLISessionID")

	// When canonical names are also present, canonical wins.
	rawBoth := `{
		"id":                  "agent-both",
		"ai_cli":              "aider",
		"backend":             "claude",
		"ai_cli_session_id":   "new-id",
		"claude_session_id":   "old-id",
		"status":              "working"
	}`
	var b Agent
	require.NoError(t, json.Unmarshal([]byte(rawBoth), &b))
	require.Equal(t, "aider", b.AiCli, "canonical ai_cli must win over backend alias")
	require.Equal(t, "new-id", b.AICLISessionID, "canonical ai_cli_session_id must win over claude_session_id alias")
}

// TestMarshalEmitsLegacyAliases verifies that serializing an Agent with the
// canonical fields also emits the deprecated alias keys during the alias window.
func TestMarshalEmitsLegacyAliases(t *testing.T) {
	a := Agent{
		ID:             "agent-emit",
		AiCli:          "aider",
		AICLISessionID: "session-xyz",
	}
	b, err := json.Marshal(a)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	require.Equal(t, "aider", out["ai_cli"], "canonical ai_cli must be present")
	require.Equal(t, "aider", out["backend"], "legacy backend must be present during alias window")
	require.Equal(t, "session-xyz", out["ai_cli_session_id"], "canonical ai_cli_session_id must be present")
	require.Equal(t, "session-xyz", out["claude_session_id"], "legacy claude_session_id must be present during alias window")
}

// TestMigrationPreservesAgentStatus verifies that active sessions whose Status
// is non-zero (e.g. done) are imported with their status intact.
func TestMigrationPreservesAgentStatus(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	// An agent that finished but has not been archived yet.
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-done", Status: store.StatusDone}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	got, err := agents.Get(ctx, "agent-done")
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, got.Status, "migration must preserve Status from legacy record")
}

// TestMigrationSkipsTerminalByKindField verifies that legacy records whose raw
// "kind" field is "terminal" are excluded even when the full decode succeeds.
func TestMigrationSkipsTerminalByKindField(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "term-1", Kind: store.KindTerminal, Status: store.StatusIdle}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{ID: "agent-1", Status: store.StatusWorking}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })
	list, err := agents.List(ctx)
	require.NoError(t, err)
	ids := make([]string, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	require.NotContains(t, ids, "term-1", "terminal session must not appear in agent store")
	require.Contains(t, ids, "agent-1", "agent session must be migrated")
}

func TestArchiveAndListClosed(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	a := &Agent{ID: "agent-arch", Name: "archiver", Status: store.StatusWorking}
	require.NoError(t, s.Insert(ctx, a))

	// GetByNameOrID finds active agent by name or ID
	byName, err := s.GetByNameOrID(ctx, "archiver")
	require.NoError(t, err)
	require.Equal(t, a.ID, byName.ID)

	byID, err := s.GetByNameOrID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.ID, byID.ID)

	// Archive moves to closed collection
	require.NoError(t, s.Archive(ctx, a.ID))
	_, err = s.Get(ctx, a.ID)
	require.ErrorIs(t, err, ErrNotFound)

	closed, err := s.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, a.ID, closed[0].ID)
	require.Equal(t, a.Status, closed[0].Status)
	require.Equal(t, a.UpdatedAt, closed[0].UpdatedAt)

	closedDegraded, skipped, err := s.ListClosedDegraded(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, skipped)
	require.Len(t, closedDegraded, 1)
}

func TestUpdateStatusIfAndFinalizeExit(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()

	a := &Agent{ID: "agent-cas", Status: store.StatusWorking}
	require.NoError(t, s.Insert(ctx, a))

	// Mismatched expected status returns false
	swapped, err := s.UpdateStatusIf(ctx, a.ID, store.StatusIdle, store.StatusDone)
	require.NoError(t, err)
	require.False(t, swapped)

	// Matched expected status swaps
	swapped, err = s.UpdateStatusIf(ctx, a.ID, store.StatusWorking, store.StatusIdle)
	require.NoError(t, err)
	require.True(t, swapped)

	got, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, got.Status)

	// FinalizeExit
	swapped, err = s.FinalizeExit(ctx, a.ID, store.StatusIdle, store.StatusDone, 1)
	require.NoError(t, err)
	require.True(t, swapped)

	got, err = s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDone, got.Status)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, 1, *got.ExitCode)
	require.Len(t, got.Events, 1)
}

func TestSessionConverters(t *testing.T) {
	a := &Agent{
		ID:             "agent-conv",
		Name:           "conv",
		Type:           store.TypeAnalysis,
		AiCli:          "claude",
		AICLISessionID: "aicli-123",
		Status:         store.StatusWorking,
		Tags:           []string{"tag1"},
	}
	sess := a.ToSession()
	require.Equal(t, a.ID, sess.ID)
	require.Equal(t, a.AiCli, sess.AiCli)
	require.Equal(t, a.AICLISessionID, sess.AICLISessionID)

	back := FromSession(sess)
	require.Equal(t, a.ID, back.ID)
	require.Equal(t, a.AiCli, back.AiCli)
	require.Equal(t, a.AICLISessionID, back.AICLISessionID)
}

func TestArchiveUpgradeAfterActiveMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	for _, a := range []*store.Session{
		{ID: "archived", Status: store.StatusOrphaned, AICLISessionID: "resume-id", ProjectID: "project", Workdir: "/work", TmuxSession: "archived"},
		{ID: "shell", Kind: store.KindTerminal, Status: store.StatusOrphaned},
	} {
		require.NoError(t, legacy.Insert(ctx, a))
		require.NoError(t, legacy.Archive(ctx, a.ID))
	}
	require.NoError(t, legacy.Close(ctx))
	// Model an installation which already completed the active-only migration.
	require.NoError(t, os.WriteFile(filepath.Join(dir, importedMarker), []byte("done"), 0600))
	s, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, s.Insert(ctx, &Agent{ID: "new-live", Subject: "must survive archive import"}))
	closed, err := s.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, "archived", closed[0].ID)
	require.Equal(t, store.StatusOrphaned, closed[0].Status)
	require.Equal(t, "resume-id", closed[0].AICLISessionID)
	require.Equal(t, "project", closed[0].ProjectID)
	// Restore, change, and archive again: latest state must replace the old copy.
	require.NoError(t, s.Insert(ctx, closed[0]))
	require.NoError(t, s.Update(ctx, "archived", func(a *Agent) error { a.Subject = "latest"; return nil }))
	require.NoError(t, s.Archive(ctx, "archived"))
	require.NoError(t, s.Close())
	// Simulate an interrupted archive import (marker absent after data was copied).
	require.NoError(t, os.Remove(filepath.Join(dir, closedImportedMarker)))
	for i := 0; i < 2; i++ {
		s, err = New(dir)
		require.NoError(t, err)
		live, err := s.Get(ctx, "new-live")
		require.NoError(t, err)
		require.Equal(t, "must survive archive import", live.Subject)
		closed, err = s.ListClosed(ctx)
		require.NoError(t, err)
		require.Len(t, closed, 1)
		require.Equal(t, "latest", closed[0].Subject)
		require.NoError(t, s.Close())
	}
}

func TestLifecycleMutationsPreserveLegacySemantics(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	changed, err := s.UpdateStatusIf(ctx, "missing", store.StatusWorking, store.StatusDone)
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = s.FinalizeExit(ctx, "missing", store.StatusWorking, store.StatusDone, 1)
	require.NoError(t, err)
	require.False(t, changed)
	a := &Agent{ID: "worker", Status: store.StatusWorking}
	require.NoError(t, s.Insert(ctx, a))
	require.NoError(t, s.SetRateLimit(ctx, a.ID, time.Now().Add(time.Hour), 1))
	first, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetRateLimit(ctx, a.ID, time.Now().Add(2*time.Hour), 2))
	second, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, first.RateLimitedAt, second.RateLimitedAt)
	require.Len(t, second.Events, 2)
	require.NoError(t, s.ClearRateLimit(ctx, a.ID))
	require.NoError(t, s.UpdateContext(ctx, a.ID, 90000, "warning"))
	require.NoError(t, s.UpdateContext(ctx, a.ID, 91000, "warning"))
	got, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, got.RateLimitedAt)
	require.Len(t, got.Events, 4)
	require.Equal(t, "rate-limit-resumed", got.Events[2].Type)
	require.Equal(t, "context none→warning (90k)", got.Events[3].Detail)
	require.ErrorIs(t, s.SetSessionID(ctx, a.ID, "bad;ref"), store.ErrBadSessionRef)
}

// TestRoleBackfillFromLegacyType verifies that records persisted with a legacy
// Type but no Role return the canonical Role on read (lazy migration).
func TestRoleBackfillFromLegacyType(t *testing.T) {
	cases := []struct {
		typ  store.Type
		role string
	}{
		{store.TypePRReview, "reviewer"},
		{store.TypeCodeReview, "reviewer"},
		{store.TypeDevelopment, "implementer"},
		{store.TypeCode, "implementer"},
		{store.TypeDocs, "implementer"},
		{store.TypeWebsite, "implementer"},
		{store.TypeDebugCI, "implementer"},
		{store.TypeTests, "implementer"},
		{store.TypeMergePR, "implementer"},
		{store.TypeRelease, "implementer"},
		{store.TypeMonitorCI, "implementer"},
		{store.TypeAnalysis, "general"},
		{store.TypeSpike, "general"},
		{store.TypeResearch, "general"},
		{store.TypeArchitecture, "general"},
		{store.TypeDesign, "general"},
	}
	for _, tc := range cases {
		t.Run(string(tc.typ), func(t *testing.T) {
			s, err := New(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			ctx := context.Background()
			id := "agent-" + string(tc.typ)
			require.NoError(t, s.Insert(ctx, &Agent{
				ID: id, Status: store.StatusWorking, Type: tc.typ,
				// Role intentionally absent: simulates a legacy record
			}))
			got, err := s.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, tc.role, got.Role, "Role must be backfilled from Type=%q", tc.typ)
		})
	}
}

// TestRoleBackfillDoesNotOverwriteExplicitRole verifies that a record with an
// explicit Role is never overwritten by the Type→Role backfill.
func TestRoleBackfillDoesNotOverwriteExplicitRole(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	require.NoError(t, s.Insert(ctx, &Agent{
		ID:     "agent-explicit-role",
		Status: store.StatusWorking,
		Type:   store.TypeDevelopment,
		Role:   "orchestrator",
	}))
	got, err := s.Get(ctx, "agent-explicit-role")
	require.NoError(t, err)
	require.Equal(t, "orchestrator", got.Role, "explicit Role must not be overwritten by Type backfill")
}

// TestRoleBackfillFromLegacyMigration verifies that legacy Session records with
// a Type but no Role are backfilled at import time.
func TestRoleBackfillFromLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "pr-agent", Status: store.StatusWorking, Type: store.TypePRReview,
	}))
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "dev-agent", Status: store.StatusWorking, Type: store.TypeDevelopment,
	}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, agents.Close()) })

	pr, err := agents.Get(ctx, "pr-agent")
	require.NoError(t, err)
	require.Equal(t, "reviewer", pr.Role)

	dev, err := agents.Get(ctx, "dev-agent")
	require.NoError(t, err)
	require.Equal(t, "implementer", dev.Role)
}
