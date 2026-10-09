package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/snapshot"
)

func TestRegistry_RegisterAndPlan(t *testing.T) {
	reg := NewRegistry()

	// Empty ID error
	require.Error(t, reg.Register(Migration{ID: "", From: 0, To: 1}))

	// Invalid versions: From >= To
	require.Error(t, reg.Register(Migration{ID: "bad", From: 1, To: 1}))
	require.Error(t, reg.Register(Migration{ID: "bad2", From: 2, To: 1}))

	// Valid registration
	m1 := Migration{ID: "0001", From: 0, To: 1, Kind: KindData}
	m2 := Migration{ID: "0002", From: 1, To: 2, Kind: KindSchema}
	require.NoError(t, reg.Register(m1))
	require.NoError(t, reg.Register(m2))

	// Duplicate error
	require.ErrorIs(t, reg.Register(m1), ErrDuplicateMigration)

	// Get
	got, ok := reg.Get("0001")
	require.True(t, ok)
	require.Equal(t, "0001", got.ID)

	// All
	all := reg.All()
	require.Len(t, all, 2)
	require.Equal(t, "0001", all[0].ID)
	require.Equal(t, "0002", all[1].ID)

	// Plan 0 -> 2
	plan, err := reg.Plan(0, 2)
	require.NoError(t, err)
	require.Len(t, plan, 2)
	require.Equal(t, "0001", plan[0].ID)
	require.Equal(t, "0002", plan[1].ID)

	// Plan equal
	emptyPlan, err := reg.Plan(1, 1)
	require.NoError(t, err)
	require.Empty(t, emptyPlan)

	// Downgrade error
	_, err = reg.Plan(2, 1)
	require.ErrorIs(t, err, ErrDowngrade)

	// Missing path error
	_, err = reg.Plan(0, 3)
	require.ErrorIs(t, err, ErrInvalidPath)
}

func TestRunner_Check(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	require.NoError(t, reg.Register(Migration{
		ID:   "0001",
		From: 0,
		To:   1,
		Kind: KindData,
		Check: func(env Env) []Finding {
			return []Finding{
				{Severity: SeverityAuto, Store: "test", Message: "found test data"},
			}
		},
	}))

	rn := NewRunner(reg)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0"}
	findings, err := rn.Check(env, 1)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Equal(t, SeverityAuto, findings[0].Severity)
	require.Equal(t, "test", findings[0].Store)
}

func TestRunner_Apply_AdvancesLedgerAndAppendsHistory(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	executed := false
	require.NoError(t, reg.Register(Migration{
		ID:   "0001",
		From: 0,
		To:   1,
		Kind: KindData,
		Run: func(env Env) error {
			executed = true
			return nil
		},
		Verify: func(env Env) error {
			require.True(t, executed)
			return nil
		},
	}))

	rn := NewRunner(reg)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0"}
	require.NoError(t, rn.Apply(env, 1))

	// Verify ledger state on disk
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Equal(t, "9.29.0", l.BinaryVersion)
	require.Nil(t, l.InProgress)
	require.Len(t, l.History, 1)
	require.Equal(t, 0, l.History[0].From)
	require.Equal(t, 1, l.History[0].To)
	require.Equal(t, "0001", l.History[0].Migration)
	require.False(t, l.History[0].At.IsZero())

	// Boot guard allows startup
	bootRes, err := schema.Boot(dir, "9.29.0")
	require.NoError(t, err)
	require.Equal(t, 1, bootRes.Ledger.SchemaVersion)
}

func TestRunner_VerifyFailure_LeavesJournalAndDoesNotAdvance(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	require.NoError(t, reg.Register(Migration{
		ID:   "0001",
		From: 0,
		To:   1,
		Kind: KindData,
		Run: func(env Env) error {
			return nil
		},
		Verify: func(env Env) error {
			return errors.New("post-condition check failed")
		},
	}))

	rn := NewRunner(reg)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0"}
	err := rn.Apply(env, 1)
	require.ErrorContains(t, err, "post-condition check failed")

	// Ledger must not have advanced, and InProgress journal must be set
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 0, l.SchemaVersion)
	require.NotNil(t, l.InProgress)
	require.Equal(t, "0001", l.InProgress.Migration)
	require.Empty(t, l.History)

	// Boot guard must refuse boot due to interrupted migration
	_, err = schema.Boot(dir, "9.29.0")
	var ge *schema.GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, schema.VerdictInterrupted, ge.Verdict)
}

func TestRunner_CrashAndResumeAtEachStep(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()

	stepExecuted := make(map[string]int)
	failAtStep := "step2"

	require.NoError(t, reg.Register(Migration{
		ID:   "0001",
		From: 0,
		To:   1,
		Kind: KindData,
		Steps: []Step{
			{
				Name: "step1",
				Run: func(env Env) error {
					stepExecuted["step1"]++
					return nil
				},
			},
			{
				Name: "step2",
				Run: func(env Env) error {
					stepExecuted["step2"]++
					if failAtStep == "step2" {
						return errors.New("simulated crash at step2")
					}
					return nil
				},
			},
			{
				Name: "step3",
				Run: func(env Env) error {
					stepExecuted["step3"]++
					return nil
				},
			},
		},
	}))

	rn := NewRunner(reg)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0"}

	// 1. Initial run crashes at step2
	err := rn.Apply(env, 1)
	require.ErrorContains(t, err, "simulated crash at step2")
	require.Equal(t, 1, stepExecuted["step1"])
	require.Equal(t, 1, stepExecuted["step2"])
	require.Equal(t, 0, stepExecuted["step3"])

	// 2. Ledger records step2 in InProgress
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 0, l.SchemaVersion)
	require.NotNil(t, l.InProgress)
	require.Equal(t, "0001", l.InProgress.Migration)
	require.Equal(t, "step2", l.InProgress.Step)

	// 3. schema.Boot refuses with VerdictInterrupted
	_, err = schema.Boot(dir, "9.29.0")
	var ge *schema.GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, schema.VerdictInterrupted, ge.Verdict)

	// 4. Applying again without resume is refused
	err = rn.Apply(env, 1)
	require.ErrorIs(t, err, ErrInterruptedMigration)

	// 5. Fix cause of crash and Resume
	failAtStep = ""
	require.NoError(t, rn.Resume(env))

	// step1 was not re-executed; step2 resumed and step3 executed
	require.Equal(t, 1, stepExecuted["step1"], "step1 should not be re-run on resume")
	require.Equal(t, 2, stepExecuted["step2"], "step2 ran again on resume")
	require.Equal(t, 1, stepExecuted["step3"], "step3 ran on resume")

	// 6. Ledger is now at schema 1 and InProgress is cleared
	l, err = schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Len(t, l.History, 1)

	// 7. Resume when no migration in progress returns error
	err = rn.Resume(env)
	require.ErrorIs(t, err, ErrNoInterruptedMigration)

	// 8. Boot succeeds cleanly
	bootRes, err := schema.Boot(dir, "9.29.0")
	require.NoError(t, err)
	require.Equal(t, 1, bootRes.Ledger.SchemaVersion)
}

func Test0001_LegacyImport_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	// 1. Seed legacy data for schedule
	legacySchedulePath := filepath.Join(dir, "schedules.json")
	schData := map[string]any{
		"sch-1": map[string]any{
			"id":      "sch-1",
			"name":    "daily-job",
			"cron":    "@daily",
			"enabled": true,
			"prompt":  "do daily work",
		},
	}
	schBytes, err := json.Marshal(schData)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(legacySchedulePath, schBytes, 0o600))

	// 2. Seed legacy snapshot data
	snapDir := filepath.Join(dir, "snapshots")
	require.NoError(t, os.MkdirAll(snapDir, 0o700))
	snap := &snapshot.Snapshot{
		ID:        "snap-1",
		SessionID: "sess-1",
		Message:   "checkpoint 1",
		CreatedAt: time.Now().UTC(),
		Workdir:   "/tmp/wt",
	}
	snapBytes, err := json.Marshal(snap)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "snap-1.json"), snapBytes, 0o600))

	// 3. Drop legacy sentinels
	sentinels := []string{
		".sessions-filedb-imported",
		".schedules-filedb-imported",
		".snapshots-filedb-imported",
	}
	for _, s := range sentinels {
		p := filepath.Join(dir, s)
		require.NoError(t, os.WriteFile(p, []byte("sentinel\n"), 0o600))
	}

	// 4. Run Check
	rn := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0", Context: context.Background()}
	findings, err := rn.Check(env, 1)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	// 5. Run Apply (executes Migration0001)
	require.NoError(t, rn.Apply(env, 1))

	// 6. Verify data exists in ScrivaDB stores
	// Schedule
	schStore, err := schedule.NewStore(legacySchedulePath)
	require.NoError(t, err)
	defer schStore.Close()
	gotSch, err := schStore.Get("sch-1")
	require.NoError(t, err)
	require.Equal(t, "daily-job", gotSch.Name)

	// Snapshot
	snapStore, err := snapshot.NewStore(snapDir)
	require.NoError(t, err)
	defer snapStore.Close()
	gotSnap, err := snapStore.Get("snap-1")
	require.NoError(t, err)
	require.Equal(t, "sess-1", gotSnap.SessionID)

	// 7. Verify sentinels are retired!
	for _, s := range schema.LegacySentinels {
		p := filepath.Join(dir, s)
		_, err := os.Stat(p)
		require.True(t, errors.Is(err, os.ErrNotExist), "sentinel %s should be retired", s)
	}

	// 8. Verify ledger
	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Len(t, l.History, 1)
	require.Equal(t, Migration0001ID, l.History[0].Migration)

	// 9. Second run of Apply is a no-op (Plan from 1 to 1 is empty)
	require.NoError(t, rn.Apply(env, 1))
}

func TestRunner_IdempotentReRun(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	count := 0
	require.NoError(t, reg.Register(Migration{
		ID:   "0001",
		From: 0,
		To:   1,
		Kind: KindData,
		Run: func(env Env) error {
			count++
			return nil
		},
		Verify: func(env Env) error {
			return nil
		},
	}))

	rn := NewRunner(reg)
	env := Env{DataDir: dir, BinaryVersion: "9.29.0"}
	require.NoError(t, rn.Apply(env, 1))
	require.Equal(t, 1, count)

	// Re-applying to target 1 is a no-op
	require.NoError(t, rn.Apply(env, 1))
	require.Equal(t, 1, count)

	l, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Len(t, l.History, 1)
}
