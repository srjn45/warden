package migrate

// Explicit, backup-gated legacy-store cleanup
// (docs/specs/2026-10-18-legacy-store-retirement-evidence.md §5).
//
// CleanupLegacy is the only code that ever removes retained schema-1 sources.
// It never runs automatically, re-runs the retirement gate and the integrity
// checks itself, and MOVES (never deletes) the enumerated paths into a
// quarantine directory under backups/. The operator deletes the quarantine
// manually once satisfied; until then everything is recoverable by moving the
// paths back, and after that only from the documented pre-schema2 backup.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

// QuarantinePrefix names quarantine directories under <data-dir>/backups.
const QuarantinePrefix = "legacy-quarantine-"

// Cleanup refusals. All are checked before anything is moved.
var (
	ErrCleanupNotEligible = errors.New("migrate: legacy cleanup refused: retirement criteria not met")
	ErrCleanupCanonical   = errors.New("migrate: legacy cleanup refused: warden-db is missing or not intact")
	ErrCleanupBackup      = errors.New("migrate: legacy cleanup refused: cutover backup does not verify")
	ErrCleanupUnconfirmed = errors.New("migrate: legacy cleanup refused: not confirmed")
)

// CleanupOptions controls CleanupLegacy.
type CleanupOptions struct {
	// Confirm authorizes the move. Without it (or with DryRun) nothing changes.
	Confirm bool
	// DryRun evaluates everything and reports the plan; never moves anything.
	DryRun bool
	// Actor is recorded in the audit event ("" = local operator).
	Actor         string
	BinaryVersion string
	Retirement    RetirementOptions
	// Now stamps the quarantine name and audit event (default: time.Now UTC).
	Now time.Time
}

// CleanupReport is the machine-readable outcome.
type CleanupReport struct {
	DataDir    string            `json:"data_dir"`
	DryRun     bool              `json:"dry_run"`
	Applied    bool              `json:"applied"`
	Paths      []string          `json:"paths"`
	Kept       []string          `json:"kept,omitempty"`
	Quarantine string            `json:"quarantine,omitempty"`
	Backup     string            `json:"backup,omitempty"`
	Retirement *RetirementReport `json:"retirement,omitempty"`
}

// Summary renders the report for a terminal.
func (r *CleanupReport) Summary() string {
	var b strings.Builder
	switch {
	case r.Applied:
		fmt.Fprintf(&b, "legacy cleanup: %d path(s) moved to %s\n", len(r.Paths), r.Quarantine)
	case len(r.Paths) == 0:
		b.WriteString("legacy cleanup: nothing to do (no legacy stores remain)\n")
	case r.DryRun:
		b.WriteString("legacy cleanup (dry run): would move to quarantine; nothing changed\n")
	}
	for _, p := range r.Paths {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	if len(r.Kept) > 0 {
		fmt.Fprintf(&b, "kept in place (still hold data warden-db does not): %s\n", strings.Join(r.Kept, ", "))
	}
	if r.Backup != "" {
		fmt.Fprintf(&b, "verified backup: %s\n", r.Backup)
	}
	if r.Applied {
		b.WriteString("recover by moving paths back from the quarantine, or restore from the backup; delete the quarantine manually when satisfied\n")
	}
	return b.String()
}

// CleanupCandidates enumerates the exact legacy paths cleanup would move
// (data-dir relative, slash-separated, sorted) and the migrated paths it must
// keep because part of their content is not in warden-db (snapshot transcripts).
func CleanupCandidates(dataDir string) (move, keep []string) {
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dataDir, filepath.FromSlash(s.Path))); err != nil {
			continue
		}
		if s.SkipSuffix != "" {
			keep = append(keep, s.Path)
			continue
		}
		move = append(move, s.Path)
	}
	sort.Strings(move)
	sort.Strings(keep)
	return move, keep
}

// CleanupLegacy runs the backup-gated cleanup. The returned report is non-nil
// whenever the evaluation got far enough to enumerate paths, including on
// refusal, so a caller can show what was (not) done.
func CleanupLegacy(ctx context.Context, dataDir string, opt CleanupOptions) (*CleanupReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	if opt.Retirement.Now.IsZero() {
		opt.Retirement.Now = opt.Now
	}

	ret, err := EvaluateRetirement(dataDir, opt.Retirement)
	if err != nil {
		return nil, err
	}
	rep := &CleanupReport{DataDir: dataDir, DryRun: opt.DryRun, Retirement: ret}
	if !ret.Eligible {
		return rep, fmt.Errorf("%w: unmet: %s", ErrCleanupNotEligible, strings.Join(ret.Blockers, ", "))
	}

	// Canonical authority must be present and intact before anything moves.
	if err := verifyCanonical(ctx, dataDir); err != nil {
		return rep, err
	}
	// Re-verify the immutable backup independently of the gate's own check.
	backup, err := verifyCutoverBackup(dataDir)
	if err != nil {
		return rep, err
	}
	rep.Backup = backup

	rep.Paths, rep.Kept = CleanupCandidates(dataDir)
	if len(rep.Paths) == 0 {
		return rep, nil // idempotent: already cleaned
	}
	if opt.DryRun {
		return rep, nil
	}
	if !opt.Confirm {
		return rep, ErrCleanupUnconfirmed
	}

	q := filepath.Join(dataDir, "backups", QuarantinePrefix+opt.Now.UTC().Format("20060102T150405Z"))
	if _, err := os.Lstat(q); err == nil {
		return rep, fmt.Errorf("migrate: quarantine %s already exists", q)
	}
	for _, rel := range rep.Paths {
		src := filepath.Join(dataDir, filepath.FromSlash(rel))
		dst := filepath.Join(q, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return rep, fmt.Errorf("migrate: cleanup quarantine %s: %w", rel, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return rep, fmt.Errorf("migrate: cleanup move %s (earlier moves are in %s): %w", rel, q, err)
		}
	}
	rep.Applied, rep.Quarantine = true, q
	relQ, _ := filepath.Rel(dataDir, q)

	// Audit + evidence. Both are best-effort writers; the move already happened
	// and is reported to the operator either way.
	audit.NewWriter(filepath.Join(dataDir, "audit.jsonl")).Log(audit.Event{
		Time: opt.Now, Action: audit.ActionLegacyCleanup, Actor: opt.Actor, Target: dataDir,
		Detail: map[string]string{
			"quarantine": filepath.ToSlash(relQ),
			"paths":      strings.Join(rep.Paths, ","),
			"backup":     rep.Backup,
		},
	})
	_ = adoption.Record(dataDir, adoption.Event{
		At: opt.Now, Kind: adoption.LegacyCleanup, BinaryVersion: opt.BinaryVersion,
		ToSchema: schema.Schema2, Backup: rep.Backup, Step: filepath.ToSlash(relQ),
	})
	return rep, nil
}

func verifyCanonical(ctx context.Context, dataDir string) error {
	if !schema.WardenDBPresent(dataDir) {
		return fmt.Errorf("%w: %s does not exist", ErrCleanupCanonical, wardendb.Dir(dataDir))
	}
	res, err := wardendb.Verify(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCleanupCanonical, err)
	}
	if res.Blocking > 0 {
		return fmt.Errorf("%w: %d blocking finding(s); run `warden inspect repair warden-db`", ErrCleanupCanonical, res.Blocking)
	}
	return nil
}

// verifyCutoverBackup re-verifies the ledger-recorded backup against its
// MANIFEST. A fresh install (no legacy data ever existed) has none; the
// candidate list is then empty anyway.
func verifyCutoverBackup(dataDir string) (string, error) {
	l, err := schema.Load(dataDir)
	if err != nil {
		return "", fmt.Errorf("%w: ledger unreadable: %v", ErrCleanupBackup, err)
	}
	var entry *schema.HistoryEntry
	for i := range l.History {
		h := &l.History[i]
		if h.To >= schema.Schema2 && (h.Migration == Migration0002ID || h.Migration == schema.MigrationBaselineFresh) {
			entry = h
		}
	}
	if entry == nil {
		return "", fmt.Errorf("%w: no schema-2 cutover in the ledger", ErrCleanupBackup)
	}
	if entry.Migration == schema.MigrationBaselineFresh {
		return "", nil
	}
	dir := entry.Backup
	if dir == "" {
		return "", fmt.Errorf("%w: ledger records no backup", ErrCleanupBackup)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(dataDir, filepath.FromSlash(dir))
	}
	if _, err := VerifySchema2Backup(dir); err != nil {
		return "", fmt.Errorf("%w: %v", ErrCleanupBackup, err)
	}
	return entry.Backup, nil
}
