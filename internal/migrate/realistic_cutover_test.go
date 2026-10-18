package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

var (
	rfNow   = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	rfDom   = backendusage.CapacityDomain{Provider: "claude"}
	rfFence = capacity.FenceRecord{SnapshotRevision: 1, DomainKey: rfDom.Key(), BucketKey: "5h", AgentID: "agent-live", RecoveryGeneration: 1}
)

const (
	rfProject = "/work/app"
	rfPlan    = "plan-aaaa1111"
	rfRun     = "ap-0123456789ab"
)

// seedRealisticLegacy writes a schema-1 data dir through the REAL store APIs
// (compat mode), so the fixture has the exact on-disk shapes a v10 daemon
// produces — including live agents, a non-terminal autopilot run, and the
// project/plan/pipeline/autopilot associations between them. It never touches
// anything outside dir.
func seedRealisticLegacy(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1", History: []schema.HistoryEntry{}}))
	s, err := storage.OpenLegacy(dir)
	require.NoError(t, err)
	require.Nil(t, s.DB, "fixture must be written in schema-1 compat mode")

	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	_, err = s.Projects.OpenProject(rfProject, "app", rfProject)
	require.NoError(t, err)
	_, err = s.Projects.CreateGroup(projectstore.ProjectGroup{ID: "grp-1", Name: "g", ProjectIDs: []string{rfProject}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)

	require.NoError(t, s.Plans.Create(ctx, &planstore.Plan{ID: rfPlan, ProjectID: rfProject, Name: "ship", Status: planstore.PlanStatusInProgress,
		AutopilotRunID: rfRun, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.Plans.AppendEvent(ctx, &planstore.PlanExecutionEvent{ID: "ev-1", PlanID: rfPlan, ExecutionID: "pe-1",
		Kind: planstore.EventKindExecutionStarted, OccurredAt: now}))
	require.NoError(t, s.Plans.AppendNote(ctx, &planstore.ExecutionNote{ID: "note-1", PlanID: rfPlan, ExecutionID: "pe-1",
		AgentID: "agent-live", Content: "hello", CreatedAt: now}))

	live := &agentstore.Agent{ID: "agent-live", Name: "live", Type: store.Type("worker"), TmuxSession: "warden-agent-live", AiCli: "claude",
		Repo: rfProject, Worktree: "/work/app/.worktrees/live", Branch: "feat/live", Status: store.StatusWorking, PID: 4242,
		ProjectID: rfProject, PlanID: rfPlan, PipelineID: "pipe-1", JobID: "build", AutopilotRunID: rfRun, AutopilotTaskID: "t1",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.Agents.Create(ctx, live))
	old := &agentstore.Agent{ID: "agent-old", Name: "old", Type: store.Type("worker"), TmuxSession: "warden-agent-old", Repo: rfProject, Status: store.StatusDone,
		ProjectID: rfProject, PlanID: rfPlan, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	require.NoError(t, s.Agents.Create(ctx, old))
	require.NoError(t, s.Agents.Archive(ctx, "agent-old"))

	for _, add := range []func() (projectstore.Project, error){
		func() (projectstore.Project, error) { return s.Projects.AddAgentToProject(rfProject, "agent-live") },
		func() (projectstore.Project, error) { return s.Projects.AddPipelineToProject(rfProject, "pipe-1") },
		func() (projectstore.Project, error) { return s.Projects.AddPlanToProject(rfProject, rfPlan) },
		func() (projectstore.Project, error) { return s.Projects.AddTerminalToProject(rfProject, "term-1") },
		func() (projectstore.Project, error) { return s.Projects.AddAutopilotToProject(rfProject, rfRun) },
	} {
		_, err := add()
		require.NoError(t, err)
	}
	require.NoError(t, s.Terminals.Create(ctx, &terminalstore.Terminal{ID: "term-1", ProjectID: rfProject, TmuxSession: "warden-term-1",
		Status: terminalstore.StatusRunning, CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, s.Pipelines.Create(&pipeline.Pipeline{ID: "pipe-1", Name: "pipe-1", Repo: rfProject, Status: pipeline.StatusRunning,
		ProjectID: rfProject, PlanID: rfPlan, Jobs: []pipeline.Job{
			{ID: "build", Prompt: "b", Worktree: "fresh", AgentID: "agent-live", Status: pipeline.JobStatus("running")},
			{ID: "test", Prompt: "t", Worktree: "none", DependsOn: []string{"build"}},
		}}))

	require.NoError(t, s.Autopilots.Create(ctx, &autopilotstore.Autopilot{ID: rfRun, ProjectID: rfProject, PlanID: rfPlan, Name: "AP:ship",
		ManagerAgentID: "agent-live", CreatedAt: now, UpdatedAt: now, Diagnostics: autopilotstore.Diagnostics{State: "active", Gate: "ci"}}))
	require.NoError(t, s.AutopilotRuns.Create(autopilot.RunRecord{RunID: rfRun, Name: "AP:ship", Repo: rfProject, PlanFile: "plan.yaml",
		PlanID: rfPlan, ProjectID: rfProject, State: autopilot.StateActive, CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, s.PlanExports.Upsert(ctx, &planexport.Record{ID: planexport.RecordID(rfPlan, "git@x:app.git", "main", "docs/plan.yaml"),
		PlanID: rfPlan, Repository: "git@x:app.git", TargetRef: "main", OutputPath: "docs/plan.yaml", Revision: 3, Outcome: planexport.OutcomeSuccess,
		PRURL: "https://example.invalid/pr/1", ExportedAt: now, UpdatedAt: now}))
	_, _, err = s.KnownPrompts.Learn(ctx, "claude", "2.0", knownprompts.Reading{Question: "Proceed?", Action: "enter", Options: []string{"Yes", "No"}, Affirmative: 0})
	require.NoError(t, err)
	_, err = s.Context.Set("run.note", "keep", "agent-live")
	require.NoError(t, err)
	_, err = s.Mailbox.Append(mailbox.Message{From: "agent-live", To: "agent-old", Body: "ping", TS: now})
	require.NoError(t, err)

	require.NoError(t, s.Backends.Upsert(backendstore.Backend{ID: "fakecli", Installed: true, Tier: "subscription", Enabled: true, DetectedAt: now}))
	require.NoError(t, s.Backends.SetDefault("fakecli"))
	require.NoError(t, s.Backends.SetThinkingMode("free_plus_local"))
	require.NoError(t, s.Backends.AddModel("fakecli", "sonnet", "Sonnet", backendstore.ModelTier("tier-1"), true, ""))

	require.NoError(t, s.Schedules.Create(&schedule.Schedule{ID: "nightly", Name: "nightly", Kind: schedule.Kind("cron"), Mode: schedule.Mode("agent"),
		Cron: "0 3 * * *", Enabled: true, Repo: rfProject, Prompt: "go", CreatedAt: now}))

	require.NoError(t, s.Snapshots.Put(&snapshot.Snapshot{ID: "snap-1", SessionID: "agent-live", Workdir: "/work/app/.worktrees/live",
		Branch: "feat/live", HeadSHA: "abc", CreatedAt: now}, "line1\nline2\n"))

	_, err = s.UsageSnapshots.Record(backendusage.UsageSnapshot{Domain: rfDom, ObservedAt: now, RecordedAt: now, SourceStatus: backendusage.StatusOK})
	require.NoError(t, err)
	claimed, err := s.ImpactFences.Claim(rfFence)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, s.Savings.Record(savings.Event{TS: now.Add(time.Minute), Feature: "classify", Agent: "agent-live", RawTokens: 5, KeptTokens: 5}))
	require.NoError(t, s.Savings.Record(savings.Event{TS: now, Feature: "compact", Agent: "agent-live", RawTokens: 10, KeptTokens: 4, Saved: 6}))
	require.NoError(t, s.Savings.SaveCalibration(savings.Calibration{BytesPerToken: 3.5, Samples: 2, Model: "m", UpdatedAt: now}))
	require.NoError(t, s.Spend.Record("agent-live", "claude", "sonnet", rfProject, 100, 50))
	require.NoError(t, s.Metrics.Record(metrics.Sample{TakenAt: now}))
	s.Audit.Log(audit.Event{Time: now, Action: "spawn", Target: "agent-live"})
	require.NoError(t, s.Close())

	// External artifacts the migration must leave byte-identical.
	for rel, body := range map[string]string{
		"prompts/agent-live.txt": "p", "hints/agent-live.txt": "h", "exits/agent-live.exit": "0",
		"session-logs/agent-live.log": "log", "config.yaml": "addr: x\n",
	} {
		writeFile(t, dir, rel, body)
	}
}

func TestRealisticCutover_PreservesLogicalDataset(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	externalBefore := map[string][]byte{}
	for _, rel := range []string{"audit.jsonl", "prompts/agent-live.txt", "hints/agent-live.txt", "exits/agent-live.exit",
		"session-logs/agent-live.log", "config.yaml", "snapshots/snap-1.transcript"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, err, rel)
		externalBefore[rel] = b
	}

	legacyBefore := snapshotTree(t, dir)

	rn := NewRunner(DefaultRegistry)
	require.NoError(t, rn.Apply(Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}, schema.Schema2))

	// Plan 07 cleanup is out of scope: every legacy file is byte-identical
	// after the cutover; the only new paths are the ledger rewrite, warden-db
	// and the backup.
	legacyAfter := snapshotTree(t, dir)
	for rel, want := range legacyBefore {
		if rel == schema.FileName {
			continue
		}
		require.Equal(t, want, legacyAfter[rel], "legacy file %s changed", rel)
	}
	for rel := range legacyAfter {
		if _, existed := legacyBefore[rel]; !existed {
			require.True(t, rel == schema.FileName || strings.HasPrefix(rel, schema.WardenDBDir+"/"), "unexpected new path %s", rel)
		}
	}

	for rel, want := range externalBefore {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, err, rel)
		require.Equal(t, want, got, "external artifact %s must be untouched", rel)
	}

	ctx := context.Background()
	s, err := storage.Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NotNil(t, s.DB, "schema-2 ledger must open the shared warden-db")

	live, err := s.Agents.Get(ctx, "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, live.Status, "active agent must not be marked/terminated by migration")
	require.Equal(t, 4242, live.PID)
	require.Equal(t, rfPlan, live.PlanID)
	require.Equal(t, rfRun, live.AutopilotRunID)
	closed, err := s.Agents.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)

	terms, err := s.Terminals.List(ctx)
	require.NoError(t, err)
	require.Len(t, terms, 1)

	p, err := s.Projects.Get(rfProject)
	require.NoError(t, err)
	require.Equal(t, "app", p.Name)
	require.Equal(t, []string{"agent-live"}, p.Agents, "project membership must survive the cutover")
	require.Equal(t, []string{"pipe-1"}, p.Pipelines)
	require.Equal(t, []string{rfPlan}, p.Plans)
	require.Equal(t, []string{"term-1"}, p.Terminals)
	require.Equal(t, []string{rfRun}, p.Autopilots)
	groups, err := s.Projects.ListGroups()
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Contains(t, groups[0].ProjectIDs, rfProject)

	plan, err := s.Plans.Get(ctx, rfPlan)
	require.NoError(t, err)
	require.Equal(t, rfRun, plan.AutopilotRunID)
	evs, err := s.Plans.ListAllEvents(ctx, rfPlan)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	notes, err := s.Plans.ListAllNotes(ctx, rfPlan)
	require.NoError(t, err)
	require.Len(t, notes, 1)

	pipe, err := s.Pipelines.Get("pipe-1")
	require.NoError(t, err)
	require.Len(t, pipe.Jobs, 2)
	require.Equal(t, "agent-live", pipe.Jobs[0].AgentID)

	ap, err := s.Autopilots.Get(ctx, rfRun)
	require.NoError(t, err)
	require.Equal(t, "agent-live", ap.ManagerAgentID)
	require.Equal(t, "active", ap.Diagnostics.State)

	exports, err := s.PlanExports.ListByPlan(ctx, rfPlan)
	require.NoError(t, err)
	require.Len(t, exports, 1)
	require.Equal(t, "https://example.invalid/pr/1", exports[0].PRURL)
	require.Len(t, s.KnownPrompts.List(), 1)

	run, err := s.AutopilotRuns.Get(rfRun)
	require.NoError(t, err)
	require.Equal(t, autopilot.StateActive, run.State, "non-terminal autopilot run must stay active")
	require.Equal(t, rfPlan, run.PlanID)

	latest, ok, err := s.UsageSnapshots.Latest(rfDom, rfNow, 24*time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, backendusage.StatusOK, latest.SourceStatus)
	dup, err := s.ImpactFences.AlreadyClaimed(rfFence)
	require.NoError(t, err)
	require.True(t, dup, "an imported fence must still suppress a repeated claim")

	e, err := s.Context.Get("run.note")
	require.NoError(t, err)
	require.Equal(t, "keep", e.Value)
	msgs, err := s.Mailbox.Messages("agent-old")
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	b, err := s.Backends.Get("fakecli")
	require.NoError(t, err)
	require.True(t, b.Default)
	st, err := s.Backends.Settings()
	require.NoError(t, err)
	require.Equal(t, "free_plus_local", st.InternalThinkingMode)
	m, err := s.Backends.GetModel("fakecli", "sonnet")
	require.NoError(t, err)
	require.Equal(t, "Sonnet", m.DisplayName)

	sc, err := s.Schedules.Get("nightly")
	require.NoError(t, err)
	require.True(t, sc.Enabled)

	sn, err := s.Snapshots.Get("snap-1")
	require.NoError(t, err)
	require.Equal(t, "agent-live", sn.SessionID)
	require.FileExists(t, sn.TranscriptPath, "snapshot transcript pointer must resolve to the untouched external blob")

	evts, err := s.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, evts, 2)
	require.Equal(t, "classify", evts[0].Feature, "ledger keeps append order")
	require.Equal(t, "compact", evts[1].Feature)
	cal, ok, err := s.Savings.Calibration()
	require.NoError(t, err)
	require.True(t, ok)
	require.InDelta(t, 3.5, cal.BytesPerToken, 0.001)
	sp, err := s.Spend.Sessions()
	require.NoError(t, err)
	require.Len(t, sp, 1)
	hist, err := s.Metrics.History(time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, hist, 1)

	trail, err := s.Audit.Read(audit.Filter{})
	require.NoError(t, err)
	require.NotEmpty(t, trail, "pre-migration audit history must stay readable after cutover")
}

// The updater records its own pre-swap snapshot in the ledger journal (no
// migration id) before it runs migrations. That is a rollback marker, not a
// schema-2 backup: the cutover must create its own immutable backup instead of
// trying to resume from it.
func TestRealisticCutover_IgnoresUpdaterSnapshotMarker(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	updaterSnap := filepath.Join(dir, "backups", "pre-v10.0.1-20261017T090000Z")
	require.NoError(t, os.MkdirAll(updaterSnap, 0o700))
	l, err := schema.Load(dir)
	require.NoError(t, err)
	l.InProgress = &schema.InProgress{Step: "snapshot", Snapshot: updaterSnap}
	require.NoError(t, schema.Save(dir, l))

	require.NoError(t, NewRunner(nil).Apply(Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}, schema.Schema2))

	l, err = schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Len(t, l.History, 1)
	require.Contains(t, l.History[0].Backup, Schema2BackupPrefix, "history must point at the verified schema-2 backup, not the updater marker")
}

// Crash/resume/restore on realistic data. A crash is simulated by cancelling the
// context mid-export (the journal then records the step), and by rewriting the
// ledger to the exact state a process death between "promoted" and the ledger
// commit leaves behind.
func TestRealisticCutover_InterruptedAndResumed(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	before := snapshotTree(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dies as soon as the export starts
	rn := NewRunner(DefaultRegistry)
	err := rn.Apply(Env{DataDir: dir, BinaryVersion: "11.0.0", Context: ctx}, schema.Schema2)
	require.Error(t, err)

	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.True(t, l.MigrationInterrupted())
	require.Equal(t, Migration0002ID, l.InProgress.Migration)
	require.NotEmpty(t, l.InProgress.Snapshot, "the verified backup is journaled before any live change")
	_, err = VerifySchema2Backup(filepath.Join(dir, filepath.FromSlash(l.InProgress.Snapshot)))
	require.NoError(t, err)

	// Every guard refuses to run on an interrupted journal; nothing is opened.
	_, err = schema.Boot(dir, "11.0.0")
	var ge *schema.GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, schema.VerdictInterrupted, ge.Verdict)
	_, err = storage.Open(dir)
	require.ErrorAs(t, err, &ge)
	require.ErrorIs(t, rn.Apply(Env{DataDir: dir, Context: context.Background()}, schema.Schema2), ErrInterruptedMigration)
	require.False(t, schema.WardenDBPresent(dir))

	// Resume completes with the same backup, no second backup, no duplicates.
	require.NoError(t, rn.Resume(Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}))
	l, err = schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", Schema2BackupPrefix+"*"))
	require.Len(t, backups, 1)
	s, err := storage.Open(dir)
	require.NoError(t, err)
	live, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, live.Status)
	require.NoError(t, s.Close())

	for rel, want := range before {
		if rel != schema.FileName {
			require.Equal(t, want, snapshotTree(t, dir)[rel], "legacy file %s changed", rel)
		}
	}
}

func TestRealisticCutover_CrashAfterPromotionResumesAndRestoreIsReversible(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}
	require.NoError(t, rn.Apply(env, schema.Schema2))
	done, err := schema.Load(dir)
	require.NoError(t, err)
	backup := done.History[len(done.History)-1].Backup
	require.NotEmpty(t, backup)

	// The state a process death leaves between the rename and the ledger commit:
	// schema 1 + journal at "promoted" + warden-db already in place.
	crashed := func() {
		require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1",
			InProgress: &schema.InProgress{Migration: Migration0002ID, Step: "promoted", Snapshot: backup}}))
	}
	crashed()
	require.NoError(t, rn.Resume(env))
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)

	// Same crash, but the operator restores instead: warden-db is preserved
	// under backups/ (never deleted), the ledger is schema 1 again, and the
	// cutover can simply be re-run.
	crashed()
	require.NoError(t, rn.Restore(env))
	l, err = schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.False(t, schema.WardenDBPresent(dir))
	abandoned, _ := filepath.Glob(filepath.Join(dir, "backups", "abandoned-schema2-*"))
	require.Len(t, abandoned, 1)
	legacy, err := storage.OpenLegacy(dir)
	require.NoError(t, err, "restored legacy data opens on the schema-1 stores")
	require.NoError(t, legacy.Close())

	time.Sleep(1100 * time.Millisecond) // backup dirs are second-stamped and never reused
	require.NoError(t, rn.Apply(env, schema.Schema2), "a restored dir can be cut over again")
	l, err = schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
}

// A corrupt, ambiguous or unmapped source refuses the cutover BEFORE anything
// is promoted or the ledger moves; active records are not marked or deleted.
func TestRealisticCutover_RefusalLeavesEverythingUntouched(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	writeFile(t, dir, "savings/ledger.jsonl", "{\"ts\":\"2026-10-17T09:00:00Z\"}\nnot json at all\n")
	before := snapshotTree(t, dir)

	err := NewRunner(DefaultRegistry).Apply(Env{DataDir: dir, BinaryVersion: "11.0.0", Context: context.Background()}, schema.Schema2)
	require.Error(t, err)
	require.False(t, schema.WardenDBPresent(dir))
	l, lerr := schema.Load(dir)
	require.NoError(t, lerr)
	require.Equal(t, 1, l.SchemaVersion)
	require.Equal(t, before, snapshotTree(t, dir), "a refused cutover changes no source byte")
}
