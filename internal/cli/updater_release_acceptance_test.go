package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/updater"
)

const (
	accProject = "/work/app"
	accPlan    = "plan-aaaa1111"
	accRun     = "ap-0123456789ab"
)

// seedReleaseAcceptanceLegacy creates a complete schema-1 data directory with
// real records across active agents, active autopilot run, project, plan, and pipeline.
func seedReleaseAcceptanceLegacy(t *testing.T, d string) {
	t.Helper()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1", History: []schema.HistoryEntry{}}))
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)

	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// Project & Group
	_, err = s.Projects.OpenProject(accProject, "app", accProject)
	require.NoError(t, err)
	_, err = s.Projects.CreateGroup(projectstore.ProjectGroup{ID: "grp-1", Name: "group-1", ProjectIDs: []string{accProject}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)

	// Plan with InProgress status, events, and notes
	require.NoError(t, s.Plans.Create(ctx, &planstore.Plan{
		ID: accPlan, ProjectID: accProject, Name: "ship-feature", Status: planstore.PlanStatusInProgress,
		AutopilotRunID: accRun, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, s.Plans.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		ID: "ev-1", PlanID: accPlan, ExecutionID: "pe-1", Kind: planstore.EventKindExecutionStarted, OccurredAt: now,
	}))
	require.NoError(t, s.Plans.AppendNote(ctx, &planstore.ExecutionNote{
		ID: "note-1", PlanID: accPlan, ExecutionID: "pe-1", AgentID: "agent-live", Content: "working on acceptance", CreatedAt: now,
	}))

	// Active agent in Working status
	live := &agentstore.Agent{
		ID: "agent-live", Name: "live", Type: store.Type("worker"), TmuxSession: "warden-live",
		Repo: accProject, Worktree: "/work/app/.worktrees/live", Branch: "feat/live", Status: store.StatusWorking, PID: 4242,
		ProjectID: accProject, PlanID: accPlan, PipelineID: "pipe-1", JobID: "build", AutopilotRunID: accRun, AutopilotTaskID: "t1",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.Agents.Create(ctx, live))

	// Archived agent
	old := &agentstore.Agent{
		ID: "agent-old", Name: "old", Type: store.Type("worker"), TmuxSession: "warden-old", Repo: accProject, Status: store.StatusDone,
		ProjectID: accProject, PlanID: accPlan, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	require.NoError(t, s.Agents.Create(ctx, old))
	require.NoError(t, s.Agents.Archive(ctx, "agent-old"))

	// Terminal
	require.NoError(t, s.Terminals.Create(ctx, &terminalstore.Terminal{
		ID: "term-1", ProjectID: accProject, TmuxSession: "warden-term-1", Status: terminalstore.StatusRunning, CreatedAt: now, UpdatedAt: now,
	}))

	// Pipeline
	require.NoError(t, s.Pipelines.Create(&pipeline.Pipeline{
		ID: "pipe-1", Name: "pipeline-1", Repo: accProject, Status: pipeline.StatusRunning, ProjectID: accProject, PlanID: accPlan,
		Jobs: []pipeline.Job{
			{ID: "build", Prompt: "build", Worktree: "fresh", AgentID: "agent-live", Status: pipeline.JobStatus("running")},
			{ID: "test", Prompt: "test", Worktree: "none", DependsOn: []string{"build"}},
		},
	}))

	// Autopilot run in Active state
	require.NoError(t, s.Autopilots.Create(ctx, &autopilotstore.Autopilot{
		ID: accRun, ProjectID: accProject, PlanID: accPlan, Name: "AP:ship", ManagerAgentID: "agent-live",
		CreatedAt: now, UpdatedAt: now, Diagnostics: autopilotstore.Diagnostics{State: "active", Gate: "ci"},
	}))
	require.NoError(t, s.AutopilotRuns.Create(autopilot.RunRecord{
		RunID: accRun, Name: "AP:ship", Repo: accProject, PlanFile: "plan.yaml", PlanID: accPlan, ProjectID: accProject,
		State: autopilot.StateActive, CreatedAt: now, UpdatedAt: now,
	}))

	// Context and Audit
	_, err = s.Context.Set("session.note", "active", "agent-live")
	require.NoError(t, err)
	s.Audit.Log(audit.Event{Time: now, Action: "spawn", Target: "agent-live"})

	require.NoError(t, s.Close())

	// External runtime files
	for rel, body := range map[string]string{
		"prompts/agent-live.txt":      "prompt content",
		"hints/agent-live.txt":        "hint content",
		"exits/agent-live.exit":       "0",
		"session-logs/agent-live.log": "log output",
		"config.yaml":                 "addr: 127.0.0.1:4040\n",
	} {
		p := filepath.Join(d, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
}

func healStoreLiveLag(t *testing.T, storeDir, colName string) {
	t.Helper()
	db, err := engine.Open(storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	_, err = db.Collection(colName)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func createContextLiveLag(t *testing.T, storeDir, colName string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(storeDir, 0o700))
	db, err := engine.Open(storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	col, err := db.Collection(colName)
	require.NoError(t, err)
	id, _, err := col.InsertWithKey("live_lag_key", map[string]any{"val": "v1"})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	colDir := filepath.Join(storeDir, colName)
	segPath := filepath.Join(colDir, "seg_000001.ndjson")
	line2 := fmt.Sprintf(`{"id":%d,"op":"update","rev":2,"ts":"2026-10-18T10:01:00Z","data":{"_key":"live_lag_key","val":"v2"}}`+"\n", id)
	f, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(line2)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// 1. Happy path: exercises active daemon preflight, full schema-1 records, live
// index lag, explicit quarantine of malformed savings rows, post-stop strict
// cutover, and verified schema-2 runtime state.
func TestReleaseRegressionAcceptance_UpdaterReportedPath_ExplicitQuarantineAndCutover(t *testing.T) {
	d := t.TempDir()
	seedReleaseAcceptanceLegacy(t, d)

	// Add live index lag to context store
	createContextLiveLag(t, filepath.Join(d, "context"), "context")

	// Add savings/ledger.jsonl with malformed records with intact line boundaries
	line1 := `{"ts":"2026-10-17T09:00:00Z","feature":"compact","raw_tokens":10,"kept_tokens":4,"saved":6,"agent":"agent-live"}` + "\n"
	line2 := `{"malformed_savings_record": broken json line` + "\n"
	line3 := `{"ts":"2026-10-17T09:05:00Z","feature":"classify","raw_tokens":5,"kept_tokens":5,"saved":0,"agent":"agent-live"}` + "\n"
	line4 := `garbled raw savings text without valid json structure` + "\n"
	line5 := `{"ts":"2026-10-17T09:10:00Z","feature":"compact","raw_tokens":20,"kept_tokens":10,"saved":10,"agent":"agent-live"}` + "\n"
	require.NoError(t, os.MkdirAll(filepath.Join(d, "savings"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(d, "savings", "ledger.jsonl"), []byte(line1+line2+line3+line4+line5), 0o600))

	expectedRawLine2 := strings.TrimRight(line2, "\r\n")
	sum2 := sha256.Sum256([]byte(expectedRawLine2))
	expectedSHA2 := hex.EncodeToString(sum2[:])

	expectedRawLine4 := strings.TrimRight(line4, "\r\n")
	sum4 := sha256.Sum256([]byte(expectedRawLine4))
	expectedSHA4 := hex.EncodeToString(sum4[:])

	// Start active daemon holder in a separate process
	holder := startOwnerHolder(t, d)

	// Target preflight check while daemon is running
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.Empty(t, res.Blockers, "preflight must not block on active daemon, live index lag, or quarantineable savings rows: %+v", res.Blockers)
	require.NotEmpty(t, res.Notes, "must report auto-recoverable and quarantine notes")

	var foundOwnerNote, foundQuarantineNote, foundCutoverNote bool
	for _, n := range res.Notes {
		if strings.Contains(n, "a running daemon is expected here") {
			foundOwnerNote = true
		}
		if strings.Contains(n, "quarantined during cutover") {
			foundQuarantineNote = true
		}
		if strings.Contains(n, "schema-2 cutover will run after the binary swap") {
			foundCutoverNote = true
		}
	}
	require.True(t, foundOwnerNote, "notes must explain running daemon is expected: %+v", res.Notes)
	require.True(t, foundQuarantineNote, "notes must mention savings quarantine: %+v", res.Notes)
	require.True(t, foundCutoverNote, "notes must mention cutover pending: %+v", res.Notes)

	// Stop daemon and heal live index lag (simulating graceful daemon shutdown)
	_ = holder.Process.Kill()
	_ = holder.Wait()
	healStoreLiveLag(t, filepath.Join(d, "context"), "context")

	// Post-stop cutover: run update migration hook
	calls := stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, runUpdateMigrations(context.Background(), cfg, d, "/fake/warden", io.Discard))
	require.Equal(t, 1, *calls)

	// Verify schema ledger advanced to 2
	l, err := schema.Load(d)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Equal(t, schema.VerdictStart, schema.Decide(l.SchemaVersion, schema.Schema2, 1, false))

	// Verify schema-2 database via storage.Open
	s, err := storage.OpenSchema2ForMigrationVerification(d)
	require.NoError(t, err)
	defer s.Close()
	require.NotNil(t, s.DB)

	// Active agent preserved
	liveAgent, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, liveAgent.Status)
	require.Equal(t, 4242, liveAgent.PID)
	require.Equal(t, accPlan, liveAgent.PlanID)
	require.Equal(t, accRun, liveAgent.AutopilotRunID)

	// Active autopilot run preserved
	activeRun, err := s.AutopilotRuns.Get(accRun)
	require.NoError(t, err)
	require.Equal(t, autopilot.StateActive, activeRun.State)
	require.Equal(t, accPlan, activeRun.PlanID)

	// Plan preserved
	plan, err := s.Plans.Get(context.Background(), accPlan)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, plan.Status)

	// Valid savings events: exactly 3 valid events imported
	savingsEvts, err := s.Savings.Events(time.Time{})
	require.NoError(t, err)
	require.Len(t, savingsEvts, 3)
	require.Equal(t, 6, savingsEvts[0].Saved)
	require.Equal(t, 0, savingsEvts[1].Saved)
	require.Equal(t, 10, savingsEvts[2].Saved)

	// Verify quarantine directory and manifest
	quarDir := filepath.Join(d, "quarantine", "savings")
	require.DirExists(t, quarDir)

	manifestBytes, err := os.ReadFile(filepath.Join(quarDir, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		SourcePath   string                      `json:"source_path"`
		TotalRecords int                         `json:"total_records"`
		Records      []migrate.QuarantinedRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, "savings/ledger.jsonl", manifest.SourcePath)
	require.Equal(t, 2, manifest.TotalRecords)
	require.Len(t, manifest.Records, 2)

	require.Equal(t, 2, manifest.Records[0].Line)
	require.Equal(t, expectedSHA2, manifest.Records[0].SHA256)
	raw2, err := os.ReadFile(filepath.Join(quarDir, fmt.Sprintf("line_%06d_offset_%012d_%s.raw", manifest.Records[0].Line, manifest.Records[0].Offset, expectedSHA2[:8])))
	require.NoError(t, err)
	require.Equal(t, expectedRawLine2, string(raw2))

	require.Equal(t, 4, manifest.Records[1].Line)
	require.Equal(t, expectedSHA4, manifest.Records[1].SHA256)
	raw4, err := os.ReadFile(filepath.Join(quarDir, fmt.Sprintf("line_%06d_offset_%012d_%s.raw", manifest.Records[1].Line, manifest.Records[1].Offset, expectedSHA4[:8])))
	require.NoError(t, err)
	require.Equal(t, expectedRawLine4, string(raw4))

	// Verify immutable pre-schema2 backup created
	backups, err := filepath.Glob(filepath.Join(d, "backups", migrate.Schema2BackupPrefix+"*"))
	require.NoError(t, err)
	require.Len(t, backups, 1)
}

// 2. Safe refusal: non-quarantineable corruption (line exceeding 16MiB) safely
// refuses before any changes, diagnostic points directly to savings/ledger.jsonl
// without suggesting broad repair or live-wins, leaving legacy data untouched.
func TestReleaseRegressionAcceptance_UpdaterReportedPath_SafeRefusalAndGuidance(t *testing.T) {
	d := t.TempDir()
	seedReleaseAcceptanceLegacy(t, d)

	// Write savings/ledger.jsonl with line exceeding 16MiB
	require.NoError(t, os.MkdirAll(filepath.Join(d, "savings"), 0o700))
	corruptPath := filepath.Join(d, "savings", "ledger.jsonl")
	f, err := os.OpenFile(corruptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	chunk := bytes.Repeat([]byte("x"), 1024*1024)
	for i := 0; i < 17; i++ {
		_, err = f.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	// Start active daemon holder
	holder := startOwnerHolder(t, d)

	// Preflight check
	findings, err := migrate.NewRunner(nil).Check(migrate.Env{DataDir: d, Context: context.Background()}, schema.Schema2)
	require.NoError(t, err)

	res := findingsToPreflightResult(findings)
	require.NotEmpty(t, res.Blockers, "corrupt JSONL must block preflight")

	var foundSavingsBlocker bool
	for _, b := range res.Blockers {
		if strings.Contains(b, "savings/ledger.jsonl") {
			foundSavingsBlocker = true
			require.Contains(t, b, "16MiB limit")
		}
	}
	require.True(t, foundSavingsBlocker, "blocker must point directly to savings/ledger.jsonl: %+v", res.Blockers)
	require.Empty(t, res.RepairCommand, "must NOT recommend repair commands for corrupt JSONL")

	pe := &updater.PreflightError{Blockers: res.Blockers, RepairCommand: res.RepairCommand}
	guidanceErr := updateRecoveryGuidance(pe)
	require.Error(t, guidanceErr)

	errMsg := guidanceErr.Error()
	require.Contains(t, errMsg, "savings/ledger.jsonl")
	require.NotContains(t, errMsg, "repair backends")
	require.NotContains(t, errMsg, "live-wins")

	_ = holder.Process.Kill()
	_ = holder.Wait()

	// Zero mutation to disk
	ledger, err := schema.Load(d)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.SchemaVersion)
	require.False(t, schema.WardenDBPresent(d))
}

// 3. Post-stop strict cutover refusal:
// If a rogue owner acquires the lock post-stop, cutover strictly refuses,
// leaving legacy data untouched and recoverable.
func TestReleaseRegressionAcceptance_UpdaterReportedPath_PostStopStrictCutoverRefusalWithoutMutation(t *testing.T) {
	d := t.TempDir()
	seedReleaseAcceptanceLegacy(t, d)

	cfg := filepath.Join(t.TempDir(), "c.yaml")

	// Post-stop race: a rogue process acquires the lock
	rogue := startOwnerHolder(t, d)

	stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	err := runUpdateMigrations(context.Background(), cfg, d, "/fake/warden", io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "the previous binary and data are restored")

	_ = rogue.Process.Kill()
	_ = rogue.Wait()

	// Data dir must remain strictly at schema 1 and unmigrated
	ledger, err := schema.Load(d)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.SchemaVersion)
	require.False(t, schema.WardenDBPresent(d))

	// Legacy stores intact and accessible
	s, err := storage.OpenLegacy(d)
	require.NoError(t, err)
	defer s.Close()
	live, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, live.Status)
}
