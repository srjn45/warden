package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/srjn45/warden/internal/schema"
)

var (
	ErrInterruptedMigration   = errors.New("migrate: a previous migration was interrupted")
	ErrNoInterruptedMigration = errors.New("migrate: no migration in progress to resume")
)

// Runner drives migrations according to a Registry.
type Runner struct {
	Registry *Registry
}

// NewRunner creates a new migration Runner with registry r (or DefaultRegistry if nil).
func NewRunner(r *Registry) *Runner {
	if r == nil {
		r = DefaultRegistry
	}
	return &Runner{Registry: r}
}

// Check evaluates read-only preflight checks on pending migrations from the
// data directory's current version to targetVersion.
func (rn *Runner) Check(env Env, targetVersion int) ([]Finding, error) {
	l, err := schema.Load(env.DataDir)
	current := 0
	if err == nil {
		current = l.SchemaVersion
	} else if !errors.Is(err, schema.ErrNoLedger) {
		return nil, err
	}

	plan, err := rn.Registry.Plan(current, targetVersion)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for _, m := range plan {
		if m.Check != nil {
			findings = append(findings, m.Check(env)...)
		}
	}
	return findings, nil
}

// Apply runs all migrations from current schema version to targetVersion.
func (rn *Runner) Apply(env Env, targetVersion int) error {
	l, err := schema.Load(env.DataDir)
	if errors.Is(err, schema.ErrNoLedger) {
		// No ledger: start from schema 0
		l = &schema.Ledger{
			SchemaVersion: 0,
			BinaryVersion: env.BinaryVersion,
			History:       []schema.HistoryEntry{},
		}
	} else if err != nil {
		return err
	}

	if l.InProgress != nil {
		return fmt.Errorf("%w: %s (step %s) is in progress; run resume or restore",
			ErrInterruptedMigration, l.InProgress.Migration, l.InProgress.Step)
	}

	plan, err := rn.Registry.Plan(l.SchemaVersion, targetVersion)
	if err != nil {
		return err
	}

	for _, m := range plan {
		if err := rn.runMigration(env, l, m, ""); err != nil {
			return err
		}
	}
	return nil
}

// Resume continues an interrupted migration recorded in the ledger journal.
func (rn *Runner) Resume(env Env) error {
	l, err := schema.Load(env.DataDir)
	if err != nil {
		return err
	}
	if l.InProgress == nil {
		return ErrNoInterruptedMigration
	}

	m, ok := rn.Registry.Get(l.InProgress.Migration)
	if !ok {
		return fmt.Errorf("migrate: interrupted migration %q not found in registry", l.InProgress.Migration)
	}

	return rn.runMigration(env, l, m, l.InProgress.Step)
}

func (rn *Runner) runMigration(env Env, l *schema.Ledger, m Migration, startStep string) error {
	// 1. Write journal before starting migration
	l.InProgress = &schema.InProgress{
		Migration: m.ID,
		Step:      startStep,
	}
	if err := schema.Save(env.DataDir, l); err != nil {
		return fmt.Errorf("migrate: write journal: %w", err)
	}

	stepRecorder := func(stepName string) error {
		l.InProgress = &schema.InProgress{
			Migration: m.ID,
			Step:      stepName,
		}
		return schema.Save(env.DataDir, l)
	}
	env.StepFn = stepRecorder

	// 2. Execute steps or Run func
	if len(m.Steps) > 0 {
		startIdx := 0
		if startStep != "" {
			for i, s := range m.Steps {
				if s.Name == startStep {
					startIdx = i
					break
				}
			}
		}
		for i := startIdx; i < len(m.Steps); i++ {
			step := m.Steps[i]
			if err := stepRecorder(step.Name); err != nil {
				return err
			}
			if err := step.Run(env); err != nil {
				return fmt.Errorf("migration %s step %s failed: %w", m.ID, step.Name, err)
			}
		}
	} else if m.Run != nil {
		if err := m.Run(env); err != nil {
			return fmt.Errorf("migration %s run failed: %w", m.ID, err)
		}
	}

	// 3. Verify BEFORE ledger advances
	if m.Verify != nil {
		if err := m.Verify(env); err != nil {
			return fmt.Errorf("migration %s verify failed: %w", m.ID, err)
		}
	}

	// 4. Advance ledger atomically
	l.InProgress = nil
	l.SchemaVersion = m.To
	l.BinaryVersion = env.BinaryVersion
	l.History = append(l.History, schema.HistoryEntry{
		From:      m.From,
		To:        m.To,
		Migration: m.ID,
		At:        time.Now().UTC(),
	})
	if err := schema.Save(env.DataDir, l); err != nil {
		return fmt.Errorf("migrate: commit ledger: %w", err)
	}

	return nil
}

// RetireSentinels unlinks marker files from dataDir.
func RetireSentinels(dataDir string, sentinels []string) error {
	for _, rel := range sentinels {
		p := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
