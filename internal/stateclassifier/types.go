package stateclassifier

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
)

// Config tunes the activity/quiescence classifier.
type Config struct {
	// QuiescenceGrace is the window of terminal silence before an
	// investigation is triggered (default 1500ms).
	QuiescenceGrace time.Duration
	// Hysteresis bounds dwell and min hits before proposals commit.
	Hysteresis ptyevent.Hysteresis
	// InvestigationTimeout bounds a single investigation run.
	InvestigationTimeout time.Duration
}

// DefaultConfig is the contract default configuration.
func DefaultConfig() Config {
	return Config{
		QuiescenceGrace:      1500 * time.Millisecond,
		Hysteresis:           ptyevent.DefaultHysteresis,
		InvestigationTimeout: 2 * time.Second,
	}
}

// Stream describes a journaled PTY stream that can be inspected and emitted to.
type Stream interface {
	ID() string
	Epoch() uint64
	Journal() *ptyruntime.Journal
	Subscribe(ptyevent.Cursor) *ptyruntime.Subscription
	Screen() string
	Emit(ptyevent.Event) (uint64, error)
}

// Manager resolves targets and runtime mode.
type Manager interface {
	Mode() ptyhost.Mode
	Lookup(name string) (ptyhost.Target, bool)
	// ObserveAgents registers for AI sessions only: plain-shell terminals are
	// never delivered, so shell output cannot reach any decision path.
	ObserveAgents(ptyhost.Observer)
	IsAuthoritative(name string) bool
}

// Store provides session record reads and compare-and-swap status updates.
type Store interface {
	Get(ctx context.Context, id string) (*agentstore.Agent, error)
	UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error)
}

// BackendResolver maps an agent record to its backend adapter.
type BackendResolver func(s *agentstore.Agent) agentbackend.Backend

// ProcessInfo summarizes a process tree query.
type ProcessInfo struct {
	RootAlive         bool
	HasActiveChildren bool
	ChildCount        int
	ChildCommands     []string
}

// ProcessInspector inspects OS process liveness and child process trees.
type ProcessInspector interface {
	Inspect(ctx context.Context, rootPID int) (ProcessInfo, error)
}

// TranscriptInfo summarizes transcript turns for an agent.
type TranscriptInfo struct {
	HasTurns   bool
	TurnCount  int
	TurnEnded  bool
	TurnActive bool
}

// TranscriptInspector inspects agent transcript progress.
type TranscriptInspector interface {
	InspectTranscript(ctx context.Context, workdir, sessionID string, b agentbackend.Backend) (TranscriptInfo, error)
}

// TransitionRecord documents a committed state transition for the explainable trail.
type TransitionRecord struct {
	At       time.Time               `json:"at"`
	From     ptyevent.State          `json:"from"`
	To       ptyevent.State          `json:"to"`
	Reason   ptyevent.WaitingReason  `json:"reason,omitempty"`
	Evidence []ptyevent.EvidenceKind `json:"evidence"`
	Seq      uint64                  `json:"seq"`
}
