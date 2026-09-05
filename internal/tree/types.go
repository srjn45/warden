package tree

import (
	"encoding/json"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// NodeType represents the structural type of a tree node (spec §4).
type NodeType string

const (
	NodeTypeProject      NodeType = "project"
	NodeTypeAgent        NodeType = "agent"
	NodeTypeTerminal     NodeType = "terminal"
	NodeTypePipeline     NodeType = "pipeline"
	NodeTypeJob          NodeType = "job"
	NodeTypeAutopilotRun NodeType = "autopilot_run"
	NodeTypeManager      NodeType = "manager"
	NodeTypeGuardian     NodeType = "guardian"
	NodeTypeTask         NodeType = "task"
	NodeTypeWorker       NodeType = "worker"
)

// Node represents a single item in the project tree hierarchy (spec §3).
type Node struct {
	Type      NodeType `json:"type"`
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Status    string   `json:"status"`
	SessionID string   `json:"session_id,omitempty"`
	Detail    *Detail  `json:"detail,omitempty"`
	Children  []*Node  `json:"children,omitempty"`
}

// Detail carries light, type-specific fields for client rendering (spec §3).
type Detail struct {
	Kind      string   `json:"kind,omitempty"`
	Backend   string   `json:"backend,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Path      string   `json:"path,omitempty"`
	Slot      string   `json:"slot,omitempty"`
	Gate      string   `json:"gate,omitempty"`
	Synthetic bool     `json:"synthetic,omitempty"`
	Degraded  bool     `json:"degraded,omitempty"`
	Closed    bool     `json:"closed,omitempty"`
}

// MarshalJSON ensures DependsOn serializes as [] when empty non-nil (spec §18).
func (d Detail) MarshalJSON() ([]byte, error) {
	type Alias Detail
	if d.DependsOn != nil && len(d.DependsOn) == 0 {
		type DetailWithEmptyDependsOn struct {
			Alias
			DependsOn []string `json:"depends_on"`
		}
		return json.Marshal(DetailWithEmptyDependsOn{
			Alias:     Alias(d),
			DependsOn: []string{},
		})
	}
	return json.Marshal(Alias(d))
}

// Tree represents the top-level response envelope (spec §3).
type Tree struct {
	Roots     []*Node `json:"roots"`
	Degraded  bool    `json:"degraded,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Inputs carries snapshots of all entities needed to construct the tree (spec §2).
type Inputs struct {
	Sessions          []*store.Session            // full fleet
	Projects          []projectstore.Project      // open + closed
	Pipelines         []*pipeline.Pipeline        // all pipelines
	Autopilot         autopilot.Status            // autopilot status carrying runs, tasks, workers
	Groups            []projectstore.ProjectGroup // optional project groups
	DegradedSubtrees  []string                    // container IDs whose subsystem failed
	PipelinesDegraded bool                        // pipeline store read failure
	AutopilotDegraded bool                        // autopilot status read failure
}
