package ptyevent

import (
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the Event wire/journal version. Bump it (and keep a decoder
// for the previous one) on any incompatible change to Event or its payloads.
const SchemaVersion = 1

// Kind discriminates an Event's payload. Exactly one payload pointer matching
// Kind is set on a valid Event.
type Kind string

const (
	// KindOutput is bytes read from the PTY master (the CLI's terminal output).
	KindOutput Kind = "output"
	// KindInput is bytes written to the PTY master (keystrokes/pastes), with the
	// origin that wrote them.
	KindInput Kind = "input"
	// KindResize is a window-size change applied to the PTY.
	KindResize Kind = "resize"
	// KindLifecycle is a process/runtime lifecycle change (spawn, exit, crash,
	// recovery, attach/detach of a presentation client).
	KindLifecycle Kind = "lifecycle"
	// KindState is a committed normalized-state transition (see Step).
	KindState Kind = "state"
	// KindPrompt is a normalized prompt appearing, changing, or clearing.
	KindPrompt Kind = "prompt"
	// KindDecision is a decision the router took on a prompt (see Decide).
	KindDecision Kind = "decision"
	// KindMarker is a journal-level marker (gap, reset, snapshot, trim).
	KindMarker Kind = "marker"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindOutput, KindInput, KindResize, KindLifecycle, KindState, KindPrompt, KindDecision, KindMarker:
		return true
	}
	return false
}

// Event is one journal record for one session. Seq is assigned by the runtime's
// single per-session writer: it starts at 1, increases by exactly 1 per event
// across the session's whole life (it does NOT reset on a new Epoch), and is the
// only ordering key clients may rely on. MonoNS is a monotonic clock reading
// relative to the Epoch's start, non-decreasing within an Epoch; Wall is
// informational (it may jump).
type Event struct {
	V         int       `json:"v"`
	SessionID string    `json:"session_id"`
	Epoch     uint64    `json:"epoch"`
	Seq       uint64    `json:"seq"`
	MonoNS    int64     `json:"mono_ns"`
	Wall      time.Time `json:"wall"`
	Kind      Kind      `json:"kind"`

	Output    *Output        `json:"output,omitempty"`
	Input     *Input         `json:"input,omitempty"`
	Resize    *Resize        `json:"resize,omitempty"`
	Lifecycle *Lifecycle     `json:"lifecycle,omitempty"`
	State     *StateChange   `json:"state,omitempty"`
	Prompt    *PromptEvent   `json:"prompt,omitempty"`
	Decision  *DecisionEvent `json:"decision,omitempty"`
	Marker    *Marker        `json:"marker,omitempty"`

	// Redactions lists every span the Redactor replaced in this event's byte
	// payload (Output.Data / Input.Data / prompt evidence). An event whose
	// payload was never passed through the Redactor must not be persisted.
	Redactions []Redaction `json:"redactions,omitempty"`
	// Redacted is true once the payload has passed the redaction boundary (even
	// when nothing matched). Persistence and fan-out beyond the live attach
	// reject events with Redacted=false.
	Redacted bool `json:"redacted"`
}

// Output is a chunk of PTY output. Data never ends in the middle of a UTF-8
// code point unless Partial is set (see SplitUTF8): the runtime holds an
// incomplete trailing sequence back and prepends it to the next read.
type Output struct {
	Data []byte `json:"data"`
	// Controls summarizes the terminal control sequences found in Data (see
	// ControlScanner); consumers use it to track alt-screen, clears and titles
	// without re-parsing.
	Controls []Control `json:"controls,omitempty"`
	// Partial is true when Data was flushed with an incomplete trailing UTF-8
	// sequence (the hold-back timed out or the process exited).
	Partial bool `json:"partial,omitempty"`
}

// Input is bytes written to the PTY.
type Input struct {
	Data     []byte `json:"data,omitempty"`
	Origin   Origin `json:"origin"`
	ClientID string `json:"client_id,omitempty"`
	// DecisionID ties Warden-origin input to the DecisionEvent that produced it.
	DecisionID string `json:"decision_id,omitempty"`
	// Secret is true when the input was typed while the PTY had ECHO off (a
	// password prompt) or matched a typed-secret pattern; Data is then dropped
	// entirely and only its length is kept.
	Secret    bool `json:"secret,omitempty"`
	SecretLen int  `json:"secret_len,omitempty"`
	// InputID is the fenced client's per-client write id (never content): it lets
	// a re-adopted session drop a write a reconnecting client resends.
	InputID uint64 `json:"input_id,omitempty"`
}

// Resize is a window-size change.
type Resize struct {
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
	ClientID string `json:"client_id,omitempty"`
	Origin   Origin `json:"origin"`
}

// LifecyclePhase is a process/runtime lifecycle step.
type LifecyclePhase string

const (
	PhaseSpawned   LifecyclePhase = "spawned"   // process started under a new Epoch
	PhaseReady     LifecyclePhase = "ready"     // backend readiness marker seen (input accepted)
	PhaseExited    LifecyclePhase = "exited"    // process exited (ExitCode set)
	PhaseSignaled  LifecyclePhase = "signaled"  // process killed by a signal (Signal set)
	PhaseCrashed   LifecyclePhase = "crashed"   // runtime lost the process unexpectedly
	PhaseRecovered LifecyclePhase = "recovered" // runtime re-adopted a live process (new Epoch, same process)
	PhaseAttached  LifecyclePhase = "attached"  // a presentation client attached
	PhaseDetached  LifecyclePhase = "detached"  // a presentation client detached
)

// Valid reports whether p is a known phase.
func (p LifecyclePhase) Valid() bool {
	switch p {
	case PhaseSpawned, PhaseReady, PhaseExited, PhaseSignaled, PhaseCrashed, PhaseRecovered, PhaseAttached, PhaseDetached:
		return true
	}
	return false
}

// StartsEpoch reports whether p is a valid first event of a new Epoch.
func (p LifecyclePhase) StartsEpoch() bool { return p == PhaseSpawned || p == PhaseRecovered }

// Terminal reports whether p ends the process.
func (p LifecyclePhase) Terminal() bool {
	return p == PhaseExited || p == PhaseSignaled || p == PhaseCrashed
}

// Presentation is how a session is hosted.
type Presentation string

const (
	// PresentationDirect is a warden-owned PTY with no multiplexer.
	PresentationDirect Presentation = "direct"
	// PresentationTmux is the compatibility mode: the process runs inside a tmux
	// session (pre-runtime sessions, or `tmux attach` users). tmux is then a
	// presentation layer only — the journal is still fed from the PTY warden
	// owns (a `tmux attach` client PTY or pipe-pane), never from capture-pane.
	PresentationTmux Presentation = "tmux"
)

// Lifecycle is a process/runtime lifecycle change.
type Lifecycle struct {
	Phase        LifecyclePhase `json:"phase"`
	PID          int            `json:"pid,omitempty"`
	ExitCode     *int           `json:"exit_code,omitempty"`
	Signal       string         `json:"signal,omitempty"`
	Backend      string         `json:"backend,omitempty"`
	Presentation Presentation   `json:"presentation,omitempty"`
	ClientID     string         `json:"client_id,omitempty"`
	Detail       string         `json:"detail,omitempty"`
}

// StateChange is a committed normalized-state transition.
type StateChange struct {
	From     State          `json:"from"`
	To       State          `json:"to"`
	Reason   WaitingReason  `json:"reason,omitempty"`
	Evidence []EvidenceKind `json:"evidence"`
}

// PromptPhase is the lifecycle of one on-screen prompt.
type PromptPhase string

const (
	PromptObserved PromptPhase = "observed"
	PromptUpdated  PromptPhase = "updated" // same fingerprint, cursor/selection moved
	PromptCleared  PromptPhase = "cleared"
)

// PromptEvent carries a normalized prompt observation.
type PromptEvent struct {
	Phase  PromptPhase `json:"phase"`
	Prompt *Prompt     `json:"prompt,omitempty"` // nil only for PromptCleared
	// Fingerprint identifies the prompt that cleared when Prompt is nil.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// DecisionEvent records what the router decided for a prompt and whether the
// decision survived revalidation and was delivered.
type DecisionEvent struct {
	ID          string  `json:"id"`
	Fingerprint string  `json:"fingerprint"`
	Posture     Posture `json:"posture"`
	Action      Action  `json:"action"`
	OptionIndex int     `json:"option_index,omitempty"`
	Rule        string  `json:"rule"`
	Reason      string  `json:"reason,omitempty"`
	// DecidedAtSeq is the journal seq the decision was computed against.
	DecidedAtSeq uint64 `json:"decided_at_seq"`
	// Revalidation is "" before delivery, "ok", or the RevalidationError code
	// that aborted it.
	Revalidation string `json:"revalidation,omitempty"`
}

// MarkerType is a journal-level marker.
type MarkerType string

const (
	// MarkerGap: events FromSeq..ToSeq were lost (writer overflow); consumers
	// must resynchronize from a snapshot.
	MarkerGap MarkerType = "gap"
	// MarkerSnapshot: a screen snapshot (Snapshot) covering everything up to
	// ToSeq was taken; replay may start here.
	MarkerSnapshot MarkerType = "snapshot"
	// MarkerTrim: retention removed events up to ToSeq.
	MarkerTrim MarkerType = "trim"
	// MarkerReset: the terminal model must be reset (e.g. journal recovered
	// after a crash with an unknown screen).
	MarkerReset MarkerType = "reset"
)

// Marker is a journal-level marker.
type Marker struct {
	Type    MarkerType `json:"type"`
	FromSeq uint64     `json:"from_seq,omitempty"`
	ToSeq   uint64     `json:"to_seq,omitempty"`
	// Snapshot is the redacted screen text for MarkerSnapshot (cols×rows grid
	// serialized as lines), with the size it was taken at.
	Snapshot string `json:"snapshot,omitempty"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
}

// Validation errors. Validate wraps them with the offending field.
var (
	ErrVersion      = errors.New("unsupported event version")
	ErrSession      = errors.New("missing session id")
	ErrSeq          = errors.New("seq must be >= 1")
	ErrKind         = errors.New("unknown kind")
	ErrPayload      = errors.New("payload does not match kind")
	ErrNotRedacted  = errors.New("event has not passed the redaction boundary")
	ErrBadRedaction = errors.New("redaction span out of range")
	ErrResize       = errors.New("resize must be non-zero")
	ErrOrigin       = errors.New("unknown input origin")
	ErrSecretData   = errors.New("secret input must not carry data")
	ErrPhase        = errors.New("unknown lifecycle phase")
	ErrMonotonic    = errors.New("mono_ns must be >= 0")
)

// payloads returns the number of set payload pointers and whether the one
// matching e.Kind is set.
func (e Event) payloads() (n int, match bool) {
	set := map[Kind]bool{
		KindOutput:    e.Output != nil,
		KindInput:     e.Input != nil,
		KindResize:    e.Resize != nil,
		KindLifecycle: e.Lifecycle != nil,
		KindState:     e.State != nil,
		KindPrompt:    e.Prompt != nil,
		KindDecision:  e.Decision != nil,
		KindMarker:    e.Marker != nil,
	}
	for _, v := range set {
		if v {
			n++
		}
	}
	return n, set[e.Kind]
}

// Validate checks one event in isolation (ordering is ValidateStream's job).
// It is what the journal writer runs before persisting and what replay runs on
// every decoded record.
func (e Event) Validate() error {
	if e.V != SchemaVersion {
		return fmt.Errorf("%w: %d", ErrVersion, e.V)
	}
	if e.SessionID == "" {
		return ErrSession
	}
	if e.Seq == 0 {
		return ErrSeq
	}
	if e.MonoNS < 0 {
		return ErrMonotonic
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrKind, e.Kind)
	}
	if n, match := e.payloads(); n != 1 || !match {
		return fmt.Errorf("%w: kind=%s payloads=%d", ErrPayload, e.Kind, n)
	}
	if !e.Redacted {
		return ErrNotRedacted
	}
	switch e.Kind {
	case KindOutput:
		if err := checkSpans(e.Redactions, len(e.Output.Data)); err != nil {
			return err
		}
	case KindInput:
		if !e.Input.Origin.Valid() {
			return fmt.Errorf("%w: %q", ErrOrigin, e.Input.Origin)
		}
		if e.Input.Secret && len(e.Input.Data) > 0 {
			return ErrSecretData
		}
		if err := checkSpans(e.Redactions, len(e.Input.Data)); err != nil {
			return err
		}
	case KindResize:
		if e.Resize.Cols == 0 || e.Resize.Rows == 0 {
			return ErrResize
		}
		if !e.Resize.Origin.Valid() {
			return fmt.Errorf("%w: %q", ErrOrigin, e.Resize.Origin)
		}
	case KindLifecycle:
		if !e.Lifecycle.Phase.Valid() {
			return fmt.Errorf("%w: %q", ErrPhase, e.Lifecycle.Phase)
		}
	case KindState:
		if !e.State.To.Valid() || !e.State.From.Valid() {
			return fmt.Errorf("%w: state %q→%q", ErrPayload, e.State.From, e.State.To)
		}
		if len(e.State.Evidence) == 0 {
			return fmt.Errorf("%w: state change without evidence", ErrPayload)
		}
	case KindPrompt:
		if e.Prompt.Phase != PromptCleared {
			if e.Prompt.Prompt == nil {
				return fmt.Errorf("%w: %s prompt event without prompt", ErrPayload, e.Prompt.Phase)
			}
			if err := e.Prompt.Prompt.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkSpans(rs []Redaction, n int) error {
	for _, r := range rs {
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > n {
			return fmt.Errorf("%w: [%d,+%d) in %d bytes", ErrBadRedaction, r.Offset, r.Length, n)
		}
	}
	return nil
}
