// Package autopilotstore persists live Autopilot executor entities.
//
// An Autopilot is a disposable plan-bound executor: it has no independent task
// lifecycle or durable completion history. Those facts live on the Plan
// (PlanExecution / PlanExecutionEvent / ExecutionSummary). Autopilot exists only
// while an autopilot-mode execution is operationally live.
package autopilotstore

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("autopilot not found")
	ErrExists       = errors.New("autopilot already exists")
	ErrPlanRequired = errors.New("autopilot requires a non-empty plan_id")
)

// Autopilot is the live executor entity for plan execution mode=autopilot.
// PlanID is required; planless creation is rejected. Display Name follows the
// AP:<plan-name> convention. ManagerAgentID is the role=autopilot manager slot;
// BrainAgentID is the optional on-demand headless brain (role=brain).
type Autopilot struct {
	ID             string      `json:"id"` // ap-<12hex>, stable across restarts
	ProjectID      string      `json:"project_id"`
	PlanID         string      `json:"plan_id"` // REQUIRED
	Name           string      `json:"name"`    // AP:<plan-name>
	ManagerAgentID string      `json:"manager_agent_id,omitempty"`
	BrainAgentID   string      `json:"brain_agent_id,omitempty"`
	Diagnostics    Diagnostics `json:"diagnostics"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// Diagnostics carries operational (not task-lifecycle) state for a live
// Autopilot. Task progress and completion history belong on the Plan.
type Diagnostics struct {
	// State is the operational run state (active/starting/healing/degraded/
	// paused). It is NOT a task ledger status.
	State string `json:"state,omitempty"`
	// IntegrationBranch is the PR merge target for workers.
	IntegrationBranch string `json:"integration_branch,omitempty"`
	// Gate is the land gate mode (ci|local).
	Gate string `json:"gate,omitempty"`
	// Strategy is the merge strategy for landing.
	Strategy string `json:"strategy,omitempty"`
	// SlotScope is the stable manager slot scope (<plan-name> or similar).
	SlotScope string `json:"slot_scope,omitempty"`
	// LastError surfaces the most recent operational failure for the cockpit.
	LastError string `json:"last_error,omitempty"`
	// Repo is the absolute project checkout path (useful for recovery).
	Repo string `json:"repo,omitempty"`
	// PlanFile is the absolute plan YAML path used when the executor started.
	PlanFile string `json:"plan_file,omitempty"`
}

// DisplayName returns the canonical AP:<plan-name> display name.
func DisplayName(planName string) string {
	name := strings.TrimSpace(planName)
	if name == "" {
		return "AP:unnamed"
	}
	if strings.HasPrefix(name, "AP:") {
		return name
	}
	return "AP:" + name
}

// ValidateCreate checks that a is eligible for persistence as a live Autopilot.
func ValidateCreate(a *Autopilot) error {
	if a == nil {
		return fmt.Errorf("autopilotstore: nil autopilot")
	}
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("autopilotstore: missing id")
	}
	if strings.TrimSpace(a.PlanID) == "" {
		return ErrPlanRequired
	}
	return nil
}

// IsLiveState reports whether a legacy RunRecord operational state should
// produce a live Autopilot during migration. Registered / stopped / complete /
// disabled runs are historical — their facts go to Plan or the legacy archive,
// not into Project.Autopilots[].
func IsLiveState(state string) bool {
	switch strings.TrimSpace(state) {
	case "starting", "active", "healing", "degraded", "paused":
		return true
	default:
		return false
	}
}
