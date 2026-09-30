package planstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Legacy import — operator-invoked migration boundary only.
//
// Never called from daemon startup, PlanService list/get/run, or normal
// create/update/transition paths (docs/specs/2026-09-30-scrivadb-canonical-plans.md
// D5 / §5 / §6.5).

const (
	// LegacyImportExecutionID is the synthetic execution id used for migration
	// audit events that are not tied to a live PlanExecution.
	LegacyImportExecutionID = "legacy-import"

	// EventKindLegacyImported is recorded once per successfully imported or
	// reconciled plan (deduped by plan id + content hash).
	EventKindLegacyImported EventKind = "legacy_imported"
)

// ImportOutcome classifies one discovered legacy YAML file.
type ImportOutcome string

const (
	ImportOutcomeImported   ImportOutcome = "imported"
	ImportOutcomeSkipped    ImportOutcome = "skipped"
	ImportOutcomeConflicted ImportOutcome = "conflicted"
	ImportOutcomeError      ImportOutcome = "error"
)

// ImportOptions controls ImportLegacy behaviour.
type ImportOptions struct {
	// ReportOnly discovers and classifies candidates without mutating ScrivaDB
	// or writing audit events. Source files are never modified in either mode.
	ReportOnly bool
}

// ImportRecord is one row in an ImportReport.
type ImportRecord struct {
	FilePath     string        `json:"file_path"`
	PlanID       string        `json:"plan_id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Status       PlanStatus    `json:"status,omitempty"`
	Outcome      ImportOutcome `json:"outcome"`
	ContentHash  string        `json:"content_hash,omitempty"`
	ExistingHash string        `json:"existing_hash,omitempty"`
	ExistingRev  int64         `json:"existing_revision,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	Reconciled   bool          `json:"reconciled,omitempty"`
}

// ImportConflictError is the structured conflict for a single candidate whose
// stable identity already exists in ScrivaDB with a different content hash.
// ImportLegacy itself does not fail the batch on conflicts — it records them
// in ImportReport.Conflicted and continues.
type ImportConflictError struct {
	PlanID       string
	FilePath     string
	ExistingHash string
	IncomingHash string
	ExistingRev  int64
}

func (e *ImportConflictError) Error() string {
	if e == nil {
		return "legacy import content hash conflict"
	}
	return fmt.Sprintf("legacy import conflict for %s (%s): existing hash %s (rev %d) != incoming %s",
		e.PlanID, e.FilePath, e.ExistingHash, e.ExistingRev, e.IncomingHash)
}

// ImportReport summarises one operator-invoked ImportLegacy run.
type ImportReport struct {
	ProjectID  string         `json:"project_id"`
	RootDir    string         `json:"root_dir"`
	ReportOnly bool           `json:"report_only"`
	Imported   []ImportRecord `json:"imported"`
	Skipped    []ImportRecord `json:"skipped"`
	Conflicted []ImportRecord `json:"conflicted"`
	Errors     []ImportRecord `json:"errors"`
}

// Counts returns tallies for each outcome bucket.
func (r *ImportReport) Counts() (imported, skipped, conflicted, errored int) {
	if r == nil {
		return 0, 0, 0, 0
	}
	return len(r.Imported), len(r.Skipped), len(r.Conflicted), len(r.Errors)
}

// importYAMLDoc is the on-disk shape accepted by legacy import: pre-envelope v1
// YAML and optional export-envelope fields from later replicas.
type importYAMLDoc struct {
	SchemaVersion int    `yaml:"schema_version"`
	PlanID        string `yaml:"plan_id"`
	Revision      int64  `yaml:"revision"`
	ContentHash   string `yaml:"content_hash"`
	Lifecycle     string `yaml:"lifecycle"`

	Version     int              `yaml:"version"`
	Name        string           `yaml:"name"`
	Goal        string           `yaml:"goal"`
	Constraints []string         `yaml:"constraints,omitempty"`
	Tasks       []importYAMLTask `yaml:"tasks"`
	DoneWhen    []string         `yaml:"done_when,omitempty"`
}

type importYAMLTask struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	After  []string `yaml:"after,omitempty"`
	Status string   `yaml:"status,omitempty"`
}

type legacyCandidate struct {
	relPath string
	absPath string
	status  PlanStatus // from directory layout; may be overridden by envelope
}

// ImportLegacy discovers plans/{pending,in_progress,completed,archived}/*.yaml
// (and flat plans/*.yaml as pending) under the project root and creates or
// reconciles canonical ScrivaDB Plans by stable identity.
//
// Duplicate policy:
//   - identity absent → create (imported)
//   - identity present + content hash match → no-op (skipped)
//   - identity present + empty definition → fill definition (imported/reconciled),
//     preserving execution links and existing lifecycle timestamps
//   - identity present + non-empty differing hash → structured conflict (conflicted)
//
// Source files are never modified. ReportOnly performs discovery/classification
// only. A migration audit event is recorded for each successful import when not
// in report mode.
func (s *PlanService) ImportLegacy(ctx context.Context, projectID string, opts ImportOptions) (*ImportReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, &ValidationError{Field: "project_id", Msg: "project_id is required"}
	}
	root, err := s.root(projectID)
	if err != nil {
		return nil, err
	}

	report := &ImportReport{
		ProjectID:  projectID,
		RootDir:    root,
		ReportOnly: opts.ReportOnly,
		Imported:   []ImportRecord{},
		Skipped:    []ImportRecord{},
		Conflicted: []ImportRecord{},
		Errors:     []ImportRecord{},
	}

	candidates, err := discoverLegacyPlanFiles(root)
	if err != nil {
		return nil, err
	}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		rec := s.importOneLegacy(ctx, projectID, c, opts.ReportOnly)
		switch rec.Outcome {
		case ImportOutcomeImported:
			report.Imported = append(report.Imported, rec)
		case ImportOutcomeSkipped:
			report.Skipped = append(report.Skipped, rec)
		case ImportOutcomeConflicted:
			report.Conflicted = append(report.Conflicted, rec)
		default:
			report.Errors = append(report.Errors, rec)
		}
	}
	return report, nil
}

func discoverLegacyPlanFiles(rootDir string) ([]legacyCandidate, error) {
	plansDir := filepath.Join(rootDir, "plans")
	if _, err := os.Stat(plansDir); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	var out []legacyCandidate
	for _, sd := range statusDirs {
		dir := filepath.Join(plansDir, sd.dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			rel := filepath.Join("plans", sd.dir, e.Name())
			out = append(out, legacyCandidate{
				relPath: rel,
				absPath: filepath.Join(rootDir, rel),
				status:  sd.status,
			})
		}
	}

	flatEntries, err := os.ReadDir(plansDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range flatEntries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		rel := filepath.Join("plans", e.Name())
		out = append(out, legacyCandidate{
			relPath: rel,
			absPath: filepath.Join(rootDir, rel),
			status:  PlanStatusPending,
		})
	}
	return out, nil
}

func (s *PlanService) importOneLegacy(ctx context.Context, projectID string, c legacyCandidate, reportOnly bool) ImportRecord {
	rec := ImportRecord{
		FilePath: c.relPath,
		Status:   c.status,
		Outcome:  ImportOutcomeError,
	}

	data, err := os.ReadFile(c.absPath)
	if err != nil {
		rec.Reason = "read: " + err.Error()
		return rec
	}
	doc, err := parseImportYAML(data)
	if err != nil {
		rec.Reason = "malformed yaml: " + err.Error()
		return rec
	}

	name := strings.TrimSpace(doc.Name)
	if name == "" {
		name = stemName(filepath.Base(c.relPath))
	}
	if name == "" {
		rec.Reason = "missing plan name"
		return rec
	}
	rec.Name = name

	if doc.Version != 0 && doc.Version != 1 {
		rec.Reason = fmt.Sprintf("unsupported plan yaml version %d", doc.Version)
		return rec
	}

	status := c.status
	if env := PlanStatus(strings.TrimSpace(doc.Lifecycle)); env.Valid() {
		status = env
	}
	rec.Status = status

	tasks, progress, err := importTasksFromDoc(doc.Tasks)
	if err != nil {
		rec.Reason = err.Error()
		return rec
	}

	planID := PlanID(projectID, name)
	if envID := strings.TrimSpace(doc.PlanID); envID != "" && envID != planID {
		rec.PlanID = planID
		rec.Reason = fmt.Sprintf("envelope plan_id %q does not match stable id %q", envID, planID)
		return rec
	}
	rec.PlanID = planID

	incoming := &Plan{
		ID:          planID,
		ProjectID:   projectID,
		Name:        name,
		Goal:        doc.Goal,
		Constraints: append([]string(nil), doc.Constraints...),
		DoneWhen:    append([]string(nil), doc.DoneWhen...),
		Tasks:       tasks,
		Status:      status,
		FilePath:    c.relPath,
	}
	incomingHash := ComputeContentHash(incoming)
	rec.ContentHash = incomingHash

	existing, err := s.store.Get(ctx, planID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		rec.Reason = "store get: " + err.Error()
		return rec
	}

	if existing == nil {
		if reportOnly {
			rec.Outcome = ImportOutcomeImported
			rec.Reason = "would create"
			return rec
		}
		now := s.clock()
		mtime := fileModTime(c.absPath, now)
		incoming.CreatedAt = mtime
		incoming.UpdatedAt = now
		incoming.TaskProgress = progress
		applyLegacyLifecycleTimestamps(incoming, status, mtime, now)
		if createErr := s.store.Create(ctx, incoming); createErr != nil {
			rec.Reason = "create: " + createErr.Error()
			return rec
		}
		if auditErr := s.recordLegacyImportAudit(ctx, incoming, incomingHash); auditErr != nil {
			rec.Reason = "imported but audit failed: " + auditErr.Error()
			rec.Outcome = ImportOutcomeImported
			return rec
		}
		rec.Outcome = ImportOutcomeImported
		rec.Reason = "created"
		return rec
	}

	existingHash := ComputeContentHash(existing)
	rec.ExistingHash = existingHash
	rec.ExistingRev = existing.Revision

	if existingHash == incomingHash {
		rec.Outcome = ImportOutcomeSkipped
		rec.Reason = "content hash match"
		return rec
	}

	if !isEmptyDefinition(existing) {
		rec.Outcome = ImportOutcomeConflicted
		rec.Reason = (&ImportConflictError{
			PlanID:       planID,
			FilePath:     c.relPath,
			ExistingHash: existingHash,
			IncomingHash: incomingHash,
			ExistingRev:  existing.Revision,
		}).Error()
		return rec
	}

	// Reconcile: prior scan-only / empty definition record — fill definition
	// without clobbering execution links or existing lifecycle timestamps.
	if reportOnly {
		rec.Outcome = ImportOutcomeImported
		rec.Reconciled = true
		rec.Reason = "would reconcile empty definition"
		return rec
	}
	if updateErr := s.store.Update(ctx, planID, func(p *Plan) error {
		p.Goal = incoming.Goal
		p.Constraints = append([]string(nil), incoming.Constraints...)
		p.DoneWhen = append([]string(nil), incoming.DoneWhen...)
		p.Tasks = append([]PlanTask(nil), incoming.Tasks...)
		p.FilePath = c.relPath
		if !p.Status.Valid() {
			p.Status = status
		}
		if p.TaskProgress == nil {
			p.TaskProgress = map[string]string{}
		}
		for id, st := range progress {
			if _, ok := p.TaskProgress[id]; !ok {
				p.TaskProgress[id] = st
			}
		}
		for _, t := range p.Tasks {
			if _, ok := p.TaskProgress[t.ID]; !ok {
				p.TaskProgress[t.ID] = "pending"
			}
		}
		RefreshContentHash(p)
		if p.Revision == 0 {
			p.Revision = 1
		}
		return nil
	}); updateErr != nil {
		rec.Reason = "reconcile: " + updateErr.Error()
		return rec
	}
	got, _ := s.store.Get(ctx, planID)
	hash := incomingHash
	if got != nil {
		hash = got.ContentHash
	}
	if auditErr := s.recordLegacyImportAudit(ctx, existing, hash); auditErr != nil {
		rec.Reason = "reconciled but audit failed: " + auditErr.Error()
		rec.Outcome = ImportOutcomeImported
		rec.Reconciled = true
		return rec
	}
	rec.Outcome = ImportOutcomeImported
	rec.Reconciled = true
	rec.Reason = "reconciled empty definition"
	rec.ContentHash = hash
	return rec
}

func parseImportYAML(data []byte) (importYAMLDoc, error) {
	var doc importYAMLDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return importYAMLDoc{}, err
	}
	return doc, nil
}

func importTasksFromDoc(tasks []importYAMLTask) ([]PlanTask, map[string]string, error) {
	if len(tasks) == 0 {
		return nil, nil, errors.New("at least one task is required")
	}
	out := make([]PlanTask, 0, len(tasks))
	progress := make(map[string]string, len(tasks))
	ids := make(map[string]struct{}, len(tasks))
	for i, t := range tasks {
		id := strings.TrimSpace(t.ID)
		prompt := strings.TrimSpace(t.Prompt)
		if id == "" {
			return nil, nil, fmt.Errorf("task[%d] is missing id", i)
		}
		if prompt == "" {
			return nil, nil, fmt.Errorf("task %q is missing prompt", id)
		}
		if _, dup := ids[id]; dup {
			return nil, nil, fmt.Errorf("duplicate task id %q", id)
		}
		ids[id] = struct{}{}
		out = append(out, PlanTask{
			ID:     id,
			Prompt: t.Prompt,
			After:  append([]string(nil), t.After...),
		})
		st := strings.ToLower(strings.TrimSpace(t.Status))
		switch st {
		case "pending", "in_progress", "done", "skipped":
			progress[id] = st
		default:
			progress[id] = "pending"
		}
	}
	for _, t := range out {
		for _, dep := range t.After {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if _, ok := ids[dep]; !ok {
				return nil, nil, fmt.Errorf("task %q after-ref %q does not exist", t.ID, dep)
			}
		}
	}
	return out, progress, nil
}

func isEmptyDefinition(p *Plan) bool {
	if p == nil {
		return true
	}
	if strings.TrimSpace(p.Goal) != "" {
		return false
	}
	if len(p.Constraints) > 0 || len(p.DoneWhen) > 0 {
		return false
	}
	return len(p.Tasks) == 0
}

func applyLegacyLifecycleTimestamps(p *Plan, status PlanStatus, mtime, now time.Time) {
	if p == nil {
		return
	}
	switch status {
	case PlanStatusInProgress:
		ts := mtime
		p.StartedAt = &ts
	case PlanStatusCompleted:
		ts := mtime
		p.StartedAt = &ts
		done := now
		if !mtime.After(now) {
			done = mtime
		}
		p.CompletedAt = &done
	case PlanStatusArchived:
		ts := mtime
		p.StartedAt = &ts
		done := mtime
		p.CompletedAt = &done
		arch := now
		if !mtime.After(now) {
			arch = mtime
		}
		p.ArchivedAt = &arch
	}
}

func fileModTime(path string, fallback time.Time) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return fallback
	}
	return fi.ModTime().UTC()
}

func (s *PlanService) recordLegacyImportAudit(ctx context.Context, p *Plan, contentHash string) error {
	if p == nil || s.store == nil {
		return nil
	}
	ev := &PlanExecutionEvent{
		ID:          NewEventID(),
		DedupKey:    fmt.Sprintf("legacy-import:%s:%s", p.ID, contentHash),
		PlanID:      p.ID,
		ExecutionID: LegacyImportExecutionID,
		Kind:        EventKindLegacyImported,
		OccurredAt:  s.clock(),
		Payload: &EventPayload{
			PlanName:   p.Name,
			Goal:       p.Goal,
			TasksTotal: len(p.Tasks),
		},
	}
	return s.store.AppendEvent(ctx, ev)
}
