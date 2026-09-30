package planexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// Sync outcome kinds returned to API/MCP/CLI callers. They mirror Record.Outcome
// plus structured reasons for the failure matrix in
// docs/specs/2026-09-30-scrivadb-canonical-plans.md §4.
const (
	ReasonIdempotentReuse   = "idempotent_reuse"
	ReasonDirtyWorktreeOK   = "operator_dirty_worktree_ignored"
	ReasonGitHubAuth        = "github_auth_unavailable"
	ReasonExistingOpenPR    = "existing_open_pr"
	ReasonBranchDivergence  = "branch_divergence"
	ReasonDeletedRemoteOK   = "deleted_remote_branch_recreated"
	ReasonPathCollision     = "path_collision"
	ReasonDirtySyncWorktree = "sync_worktree_dirty"
)

// ErrSyncConflict is returned when sync cannot safely proceed (path collision
// or non-fast-forward branch divergence). Canonical Plan data is unchanged.
var ErrSyncConflict = errors.New("plan sync conflict")

// ErrGitHubAuth is returned when gh authentication is unavailable.
var ErrGitHubAuth = errors.New("github auth unavailable")

// SyncOptions are the explicit repository/export inputs for sync_to_repo.
type SyncOptions struct {
	// PlanID is required.
	PlanID string
	// RepoPath is the local git repository root to sync into. Required.
	RepoPath string
	// TargetRef is the PR base branch (e.g. main or an integration branch). Required.
	TargetRef string
	// OutputPath overrides the conventional plans/{lifecycle}/<slug>.yaml path.
	// Empty ⇒ renderer default.
	OutputPath string
	// Repository is the stable identity stored on the export record (remote URL
	// preferred). Empty ⇒ derived from `git remote get-url origin` or RepoPath.
	Repository string
	// ExportedAt pins the envelope timestamp for deterministic re-render.
	// Zero ⇒ time.Now().UTC().
	ExportedAt time.Time
}

// SyncResult is the structured outcome of one sync_to_repo attempt.
type SyncResult struct {
	PlanID       string  `json:"plan_id"`
	Revision     int64   `json:"revision"`
	ContentHash  string  `json:"content_hash"`
	Repository   string  `json:"repository"`
	TargetRef    string  `json:"target_ref"`
	OutputPath   string  `json:"output_path"`
	Branch       string  `json:"branch,omitempty"`
	CommitSHA    string  `json:"commit_sha,omitempty"`
	PRURL        string  `json:"pr_url,omitempty"`
	PRCreated    bool    `json:"pr_created,omitempty"`
	Outcome      Outcome `json:"outcome"`
	Reason       string  `json:"reason,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Reused       bool    `json:"reused"` // true ⇒ no new GitHub activity
	RecordID     string  `json:"record_id,omitempty"`
}

// PlanReader loads a canonical Plan by ID (ScrivaDB only).
type PlanReader interface {
	Get(ctx context.Context, id string) (*planstore.Plan, error)
}

// PlanExporter persists last-export metadata onto the canonical Plan.
type PlanExporter interface {
	Update(ctx context.Context, id string, fn func(*planstore.Plan) error) error
}

// Syncer runs the safe plan → dedicated-branch → PR workflow.
type Syncer struct {
	Plans    PlanReader
	Exports  RecordStore
	PlansMut PlanExporter // optional; when set, updates Plan.RepoExport on success
	Git      GitHost
	Renderer Renderer
	Now      func() time.Time
}

// SyncBranch returns the dedicated Warden branch for a plan revision.
func SyncBranch(planID string, revision int64) string {
	return fmt.Sprintf("warden/plan-sync/%s/%d", planID, revision)
}

// Sync loads the Plan from ScrivaDB, renders the requested revision, and opens
// or reuses a PR on a dedicated branch. It never checks out or stages files on
// the operator's current branch.
func (s *Syncer) Sync(ctx context.Context, opts SyncOptions) (*SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.Plans == nil || s.Exports == nil || s.Git == nil {
		return nil, fmt.Errorf("planexport: syncer is not fully configured")
	}
	opts.PlanID = strings.TrimSpace(opts.PlanID)
	opts.RepoPath = strings.TrimSpace(opts.RepoPath)
	opts.TargetRef = strings.TrimSpace(opts.TargetRef)
	opts.OutputPath = filepath.ToSlash(strings.TrimSpace(opts.OutputPath))
	if opts.PlanID == "" {
		return nil, fmt.Errorf("planexport: plan_id is required")
	}
	if opts.RepoPath == "" {
		return nil, fmt.Errorf("planexport: repository path is required")
	}
	if opts.TargetRef == "" {
		return nil, fmt.Errorf("planexport: target_ref is required")
	}
	if err := validateGitRef(opts.TargetRef); err != nil {
		return nil, err
	}

	renderer := s.Renderer
	if renderer == nil {
		renderer = Default()
	}
	now := s.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	exportedAt := opts.ExportedAt
	if exportedAt.IsZero() {
		exportedAt = now()
	}
	exportedAt = exportedAt.UTC().Truncate(time.Second)

	plan, err := s.Plans.Get(ctx, opts.PlanID)
	if err != nil {
		return nil, err
	}
	if plan.ContentHash == "" {
		plan.ContentHash = planstore.ComputeContentHash(plan)
	}

	rendered, err := renderer.Render(plan, Options{ExportedAt: exportedAt})
	if err != nil {
		return nil, err
	}
	outputPath := opts.OutputPath
	if outputPath == "" {
		outputPath = rendered.Path
	}
	outputPath = filepath.ToSlash(outputPath)
	if err := validateOutputPath(outputPath); err != nil {
		return nil, err
	}

	repository := strings.TrimSpace(opts.Repository)
	if repository == "" {
		repository, err = s.Git.RepositoryIdentity(ctx, opts.RepoPath)
		if err != nil {
			return nil, err
		}
	}

	res := &SyncResult{
		PlanID:      plan.ID,
		Revision:    plan.Revision,
		ContentHash: plan.ContentHash,
		Repository:  repository,
		TargetRef:   opts.TargetRef,
		OutputPath:  outputPath,
		Branch:      SyncBranch(plan.ID, plan.Revision),
	}

	// Idempotency: same rev/hash already successfully exported for this key.
	if prior, findErr := s.Exports.Find(ctx, plan.ID, repository, opts.TargetRef, outputPath); findErr == nil {
		if prior.SameExport(plan.Revision, plan.ContentHash) {
			res.Outcome = OutcomeSkipped
			res.Reason = ReasonIdempotentReuse
			res.Reused = true
			res.CommitSHA = prior.CommitSHA
			res.PRURL = prior.PRURL
			res.RecordID = prior.ID
			res.Branch = SyncBranch(plan.ID, plan.Revision)
			return res, nil
		}
	} else if !errors.Is(findErr, ErrRecordNotFound) {
		return nil, findErr
	}

	// Operator dirty tree must never be staged — we only use an isolated worktree.
	operatorDirty, err := s.Git.WorkingTreeDirty(ctx, opts.RepoPath)
	if err != nil {
		return nil, err
	}
	dirtyReason := ""
	if operatorDirty {
		dirtyReason = ReasonDirtyWorktreeOK
	}

	if err := s.Git.CheckGitHubAuth(ctx, opts.RepoPath); err != nil {
		fail := s.failResult(res, OutcomeFailed, ReasonGitHubAuth, err.Error())
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, fmt.Errorf("%w: %v", ErrGitHubAuth, err)
	}

	worktree, cleanup, remoteExisted, err := s.Git.EnsureSyncWorktree(ctx, opts.RepoPath, res.Branch, opts.TargetRef)
	if err != nil {
		fail := s.failResult(res, OutcomeFailed, "", err.Error())
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, err
	}
	defer cleanup()

	if dirty, derr := s.Git.WorkingTreeDirty(ctx, worktree); derr != nil {
		return nil, derr
	} else if dirty {
		fail := s.failResult(res, OutcomeFailed, ReasonDirtySyncWorktree,
			"isolated sync worktree has unexpected uncommitted changes")
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, fmt.Errorf("%w: %s", ErrSyncConflict, fail.ErrorMessage)
	}

	exists, existing, err := s.Git.ReadFile(ctx, worktree, outputPath)
	if err != nil {
		return nil, err
	}
	if exists && !isWardenExportForPlan(existing, plan.ID) {
		fail := s.failResult(res, OutcomeConflict, ReasonPathCollision,
			fmt.Sprintf("refusing to overwrite non-Warden file at %s", outputPath))
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, fmt.Errorf("%w: %s", ErrSyncConflict, fail.ErrorMessage)
	}

	needCommit := true
	if exists && bytesEqual(existing, rendered.Bytes) {
		needCommit = false
		if sha, herr := s.Git.HeadSHA(ctx, worktree); herr == nil {
			res.CommitSHA = sha
		}
	}

	if needCommit {
		if err := s.Git.WriteFile(ctx, worktree, outputPath, rendered.Bytes); err != nil {
			fail := s.failResult(res, OutcomeFailed, "", err.Error())
			_, _ = s.persistRecord(ctx, fail, exportedAt)
			return fail, err
		}
		sha, cerr := s.Git.CommitPath(ctx, worktree, outputPath, syncCommitMessage(plan.ID, plan.Revision))
		if cerr != nil {
			fail := s.failResult(res, OutcomeFailed, "", cerr.Error())
			_, _ = s.persistRecord(ctx, fail, exportedAt)
			return fail, cerr
		}
		res.CommitSHA = sha
	}

	pushErr := s.Git.PushBranch(ctx, worktree, res.Branch)
	if pushErr != nil {
		reason := ""
		outcome := OutcomeFailed
		msg := pushErr.Error()
		if isNonFastForward(msg) {
			reason = ReasonBranchDivergence
			outcome = OutcomeConflict
			fail := s.failResult(res, outcome, reason,
				"remote branch diverged; refusing to force-push a plan-sync branch")
			_, _ = s.persistRecord(ctx, fail, exportedAt)
			return fail, fmt.Errorf("%w: %s", ErrSyncConflict, fail.ErrorMessage)
		}
		fail := s.failResult(res, outcome, reason, msg)
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, pushErr
	}
	if !remoteExisted {
		res.Reason = ReasonDeletedRemoteOK
	}

	pr, prErr := s.Git.CreateOrReusePR(ctx, worktree, PRRequest{
		Base:  opts.TargetRef,
		Head:  res.Branch,
		Title: syncPRTitle(plan.Name, plan.ID, plan.Revision),
		Body:  syncPRBody(plan, outputPath, plan.ContentHash),
	})
	if prErr != nil {
		fail := s.failResult(res, OutcomeFailed, "", prErr.Error())
		_, _ = s.persistRecord(ctx, fail, exportedAt)
		return fail, prErr
	}
	res.PRURL = pr.URL
	res.PRCreated = pr.Created
	if !pr.Created {
		res.Reason = ReasonExistingOpenPR
	} else if dirtyReason != "" && res.Reason == "" {
		res.Reason = dirtyReason
	} else if dirtyReason != "" && res.Reason == ReasonDeletedRemoteOK {
		// keep deleted-remote reason; dirty isolation is implicit
	} else if dirtyReason != "" {
		res.Reason = dirtyReason
	}

	res.Outcome = OutcomeSuccess
	rec, err := s.persistRecord(ctx, res, exportedAt)
	if err != nil {
		return res, err
	}
	res.RecordID = rec.ID

	if s.PlansMut != nil {
		meta, merr := RepoExportMeta(plan, exportedAt)
		if merr == nil {
			meta.FilePath = outputPath
			_ = s.PlansMut.Update(ctx, plan.ID, func(p *planstore.Plan) error {
				p.RepoExport = meta
				if p.FilePath == "" {
					p.FilePath = outputPath
				}
				return nil
			})
		}
	}
	return res, nil
}

func (s *Syncer) failResult(base *SyncResult, outcome Outcome, reason, msg string) *SyncResult {
	out := *base
	out.Outcome = outcome
	out.Reason = reason
	out.ErrorMessage = msg
	out.Reused = false
	return &out
}

func (s *Syncer) persistRecord(ctx context.Context, res *SyncResult, exportedAt time.Time) (*Record, error) {
	rec := &Record{
		PlanID:       res.PlanID,
		Repository:   res.Repository,
		TargetRef:    res.TargetRef,
		OutputPath:   res.OutputPath,
		Revision:     res.Revision,
		ContentHash:  res.ContentHash,
		PRURL:        res.PRURL,
		CommitSHA:    res.CommitSHA,
		Outcome:      res.Outcome,
		ErrorMessage: res.ErrorMessage,
		ExportedAt:   exportedAt,
	}
	if err := s.Exports.Upsert(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func syncCommitMessage(planID string, revision int64) string {
	return fmt.Sprintf("chore(plan-export): sync %s revision %d", planID, revision)
}

func syncPRTitle(name, planID string, revision int64) string {
	if strings.TrimSpace(name) == "" {
		name = planID
	}
	return fmt.Sprintf("plan export: %s (rev %d)", name, revision)
}

func syncPRBody(plan *planstore.Plan, outputPath, contentHash string) string {
	return fmt.Sprintf(
		"## Summary\n"+
			"- Optional repository replica of canonical ScrivaDB plan `%s`\n"+
			"- Revision `%d`, content hash `%s`\n"+
			"- Path `%s` (descriptive lifecycle layout only — not authoritative)\n\n"+
			"This PR is opened by `warden plan sync_to_repo`. The YAML is an inert "+
			"replica; execution continues to read ScrivaDB only.\n",
		plan.ID, plan.Revision, contentHash, outputPath,
	)
}

func validateOutputPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return fmt.Errorf("planexport: invalid output_path %q", p)
	}
	return nil
}

func validateGitRef(ref string) error {
	if ref == "" || strings.ContainsAny(ref, " \t\n") || strings.Contains(ref, "..") {
		return fmt.Errorf("planexport: invalid target_ref %q", ref)
	}
	return nil
}

func isWardenExportForPlan(data []byte, planID string) bool {
	text := string(data)
	if !strings.Contains(text, "warden-plan-export:") {
		return false
	}
	// Accept either "plan_id: <id>" or "plan_id: '<id>'" forms.
	return strings.Contains(text, "plan_id: "+planID) ||
		strings.Contains(text, "plan_id: \""+planID+"\"") ||
		strings.Contains(text, "plan_id: '"+planID+"'")
}

func isNonFastForward(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "non-fast-forward") ||
		strings.Contains(l, "failed to push some refs") ||
		strings.Contains(l, "updates were rejected")
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Ensure parent dirs exist for WriteFile helpers used by tests and the real host.
func mkdirAllParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
