package planstore

import "time"

// BranchFate records what happened to the integration branch after a plan ended
// (plan-finish-flow §6). leftover_unmerged is never stored — it is computed at
// read time for plans completed before this field existed (§9).
type BranchFate string

const (
	BranchFateDeleted          BranchFate = "deleted"
	BranchFateKeptUnmerged     BranchFate = "kept_unmerged"
	BranchFateAbandoned        BranchFate = "abandoned"
	BranchFateDeleteFailed     BranchFate = "delete_failed"
	BranchFateLeftoverUnmerged BranchFate = "leftover_unmerged" // computed only
)

// MaxBranchDeleteAttempts is how many times the completion watcher retries a
// failed integration-branch deletion before giving up (plan-finish-flow §4).
const MaxBranchDeleteAttempts = 5

// PlanOutcomeFinalPR is the final PR (integration → default) mirrored onto the
// plan so it survives executor teardown.
type PlanOutcomeFinalPR struct {
	Number   int    `json:"number"`
	URL      string `json:"url,omitempty"`
	State    string `json:"state,omitempty"` // open | merged | closed
	HeadSHA  string `json:"head_sha,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
}

// PlanOutcome is the durable ending record written by the daemon (plan-finish-
// flow §6). It survives executor teardown and is the source for `wd plan show`.
type PlanOutcome struct {
	IntegrationBranch    string              `json:"integration_branch,omitempty"`
	DefaultBranch        string              `json:"default_branch,omitempty"`
	FinalPR              *PlanOutcomeFinalPR `json:"final_pr,omitempty"`
	BranchFate           BranchFate          `json:"branch_fate,omitempty"`
	BranchDeletedAt      *time.Time          `json:"branch_deleted_at,omitempty"`
	BranchDeleteError    string              `json:"branch_delete_error,omitempty"`
	BranchDeleteAttempts int                 `json:"branch_delete_attempts,omitempty"`
}

// NeedsBranchDeleteRetry reports whether the watcher should retry deleting the
// integration branch (delete_failed and under the attempt bound).
func (o *PlanOutcome) NeedsBranchDeleteRetry() bool {
	if o == nil {
		return false
	}
	return o.BranchFate == BranchFateDeleteFailed &&
		o.BranchDeleteAttempts > 0 &&
		o.BranchDeleteAttempts < MaxBranchDeleteAttempts &&
		o.IntegrationBranch != ""
}
