package tree

import (
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// The shared 7-value status enum (spec §5). Every node type normalizes its
// native lifecycle onto exactly one of these, so the three renderers (TUI, hub
// web, android) key off identical strings and never learn per-type vocabularies.
// Contract like serverCapabilities: append, never rename.
const (
	StatusActive  = "active"  // doing work now
	StatusWaiting = "waiting" // needs a human
	StatusIdle    = "idle"    // alive, not working
	StatusDone    = "done"    // finished ok
	StatusError   = "error"   // failed / needs attention
	StatusBlocked = "blocked" // queued / not started
	StatusUnknown = "unknown" // no lifecycle / unmapped
)

// sessionStatus maps a store.Status onto the shared enum (spec §5). A live agent
// working or spawning is active; one awaiting input is waiting; a rate-limited
// one is blocked (queued behind its limit reset); errored/orphaned are error.
func sessionStatus(s store.Status) string {
	switch s {
	case store.StatusWorking, store.StatusSpawning:
		return StatusActive
	case store.StatusWaitingForInput:
		return StatusWaiting
	case store.StatusIdle:
		return StatusIdle
	case store.StatusDone:
		return StatusDone
	case store.StatusErrored, store.StatusOrphaned:
		return StatusError
	case store.StatusRateLimited:
		return StatusBlocked
	default:
		return StatusUnknown
	}
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
		if c == StatusError {
			return StatusError
		}
		switch c {
		case StatusActive:
			anyActive = true
		case StatusWaiting:
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

// rollupNodes is rollup over a node slice — the container status from its
// children's statuses.
func rollupNodes(children []*Node) string {
	statuses := make([]string, len(children))
	for i, c := range children {
		statuses[i] = c.Status
	}
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
