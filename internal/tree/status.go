package tree

import (
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// Container, pipeline, job and run statuses retain the original tree vocabulary
// (tree spec §5). Agent nodes use store.PresentedStatus; terminals use liveness.
const (
	StatusActive  = "active"  // doing work now
	StatusWaiting = "waiting" // needs a human
	StatusIdle    = "idle"    // alive, not working
	StatusDone    = "done"    // finished ok
	StatusError   = "error"   // failed / needs attention
	StatusBlocked = "blocked" // queued / not started
	StatusUnknown = "unknown" // no lifecycle / unmapped
)

// sessionStatus presents agent states while preserving native store values.
func sessionStatus(s store.Status, exitCodes ...*int) string {
	var exitCode *int
	if len(exitCodes) > 0 {
		exitCode = exitCodes[0]
	}
	return store.PresentedStatus(s, exitCode)
}

// terminalStatus exposes liveness only, never an AI working/input state.
func terminalStatus(s store.Status, exitCodes ...*int) string {
	switch s.Canonical() {
	case store.StatusSpawning, store.StatusWorking, store.StatusIdle, store.StatusWaitingForInput, store.StatusRateLimited:
		return "busy"
	case store.StatusDone:
		return "done"
	case store.StatusErrored:
		if len(exitCodes) > 0 && exitCodes[0] != nil {
			return "done"
		}
	}
	return "orphaned"
}

// jobStatus maps a pipeline.JobStatus onto the shared enum (spec §5). A pending
// or skipped job is blocked (not started); needs_attention rolls up with failed
// as error so a client's error palette catches both.
func jobStatus(s pipeline.JobStatus) string {
	switch s {
	case pipeline.JobRunning:
		return StatusActive
	case pipeline.JobDone:
		return StatusDone
	case pipeline.JobFailed, pipeline.JobNeedsAttention:
		return StatusError
	case pipeline.JobPending, pipeline.JobSkipped:
		return StatusBlocked
	default:
		return StatusUnknown
	}
}

// pipelineStatus maps a pipeline.Status onto the shared enum (spec §5). A paused
// pipeline is idle (alive, not advancing); stalled/canceled are error.
func pipelineStatus(s pipeline.Status) string {
	switch s {
	case pipeline.StatusRunning:
		return StatusActive
	case pipeline.StatusPaused:
		return StatusIdle
	case pipeline.StatusDone:
		return StatusDone
	case pipeline.StatusStalled, pipeline.StatusCanceled:
		return StatusError
	case pipeline.StatusPending:
		return StatusBlocked
	default:
		return StatusUnknown
	}
}

// runStatus maps an autopilot run's state (+ gate) onto the shared enum (spec
// §5). A gate warning means the run is blocked on a human decision → waiting;
// otherwise the RunState machine maps: active/starting/healing → active, paused/
// disabled → idle, registered → blocked (created, not started), degraded → error,
// complete/stopped → done.
func runStatus(r autopilot.RunStatus) string {
	if r.GateWarning != "" {
		return StatusWaiting
	}
	switch r.State {
	case autopilot.StateActive, autopilot.StateStarting, autopilot.StateHealing:
		return StatusActive
	case autopilot.StatePaused, autopilot.StateDisabled:
		return StatusIdle
	case autopilot.StateRegistered:
		return StatusBlocked
	case autopilot.StateDegraded:
		return StatusError
	case autopilot.StateComplete, autopilot.StateStopped:
		return StatusDone
	default:
		return StatusUnknown
	}
}

// rollup computes a container node's status (project, task — spec §5) from its
// children's already-normalized statuses. Precedence: error > active > waiting >
// done (only if every child is done) > idle. An empty container is idle.
//
// Note: the RFC prose in §5 lists "waiting before active", but the locked worked
// example in §18 shows a project with an active and a waiting child resolving to
// "active"; §18 (the golden) is authoritative, so active wins over waiting here.
func rollup(children []string) string {
	if len(children) == 0 {
		return StatusIdle
	}
	anyActive, anyWaiting, allDone := false, false, true
	for _, c := range children {
		if c == StatusError || c == "orphaned" {
			return StatusError
		}
		switch c {
		case StatusActive, "busy", "pending":
			anyActive = true
		case StatusWaiting, "need-input":
			anyWaiting = true
		}
		if c != StatusDone {
			allDone = false
		}
	}
	switch {
	case anyActive:
		return StatusActive
	case anyWaiting:
		return StatusWaiting
	case allDone:
		return StatusDone
	default:
		return StatusIdle
	}
}

// rollupNodes is rollup over a node slice and every descendant. A project can
// contain active work beneath a waiting direct child, so considering only the
// first level would hide active work from the project's status.
func rollupNodes(children []*Node) string {
	statuses := make([]string, 0, len(children))
	var collect func([]*Node)
	collect = func(nodes []*Node) {
		for _, n := range nodes {
			statuses = append(statuses, n.Status)
			collect(n.Children)
		}
	}
	collect(children)
	return rollup(statuses)
}

// taskStatus returns the status of an autopilot task: if it has no workers,
// it is blocked (spec §5); otherwise it rolls up its workers' statuses.
func taskStatus(children []*Node) string {
	if len(children) == 0 {
		return StatusBlocked
	}
	return rollupNodes(children)
}
