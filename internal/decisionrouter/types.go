package decisionrouter

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// Stream is the journaled PTY stream interface that supports emission and screen reading.
type Stream interface {
	ID() string
	Epoch() uint64
	Journal() *ptyruntime.Journal
	Subscribe(ptyevent.Cursor) *ptyruntime.Subscription
	Screen() string
	Emit(ptyevent.Event) (uint64, error)
}

// InputWriter is the interface for delivering input to an agent's terminal.
type InputWriter interface {
	WriteInput(ctx context.Context, data []byte, origin ptyevent.Origin, decisionID string) error
	Lease() ptyevent.Lease
}

// HumanInboxNotifier notifies human operators of decisions requiring human action.
type HumanInboxNotifier interface {
	NotifyHumanApproval(ctx context.Context, agentID, sessionID string, p *ptyevent.Prompt, safeOptions []string) error
}

// ClassifyRequest provides all contextual data for classifying a prompt.
type ClassifyRequest struct {
	Screen            string
	Backend           agentbackend.Backend
	BackendID         string
	CLIVersion        string
	SessionID         string
	ProjectID         string
	Workdir           string
	TargetInWorkspace bool
	AgentID           string
	Now               time.Time
	// Deterministic restricts classification to the backend parser: no
	// Fast-Brain call, no learned-shape lookup. Used to re-read the live screen
	// right before input is written.
	Deterministic bool
}

// ClassifyResult is the result of prompt classification.
type ClassifyResult struct {
	Prompt         *ptyevent.Prompt
	CandidateShape *ptyevent.LearnedShape
	// Recognition is the (redacted-input) Fast-Brain reading behind a
	// fastbrain-provenance prompt; the Learner freezes it into a fixture.
	Recognition     *fastbrain.Recognition
	IsInformational bool
	RawScreen       string
}

// Classifier classifies on-screen prompts and extracts capabilities.
type Classifier interface {
	Classify(ctx context.Context, req ClassifyRequest) (ClassifyResult, error)
}

// LearnedShapeStore manages the lifecycle of learned prompt shapes.
type LearnedShapeStore interface {
	Get(id string) (ptyevent.LearnedShape, bool)
	GetByFingerprint(fp string) (ptyevent.LearnedShape, bool)
	AddCandidate(shape ptyevent.LearnedShape) (ptyevent.LearnedShape, error)
	RecordObservation(fp string, fixtureID string, humanConfirmed, parserAgreed bool) (ptyevent.LearnedShape, error)
	Validate(id string, now time.Time, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error)
	Pin(id string, now time.Time, cliVersion string, kind ptyevent.ShapeScopeKind, scopeID string, ttl time.Duration, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error)
	Retire(id string, now time.Time, reason string) (ptyevent.LearnedShape, error)
	FindUsable(q ptyevent.ShapeQuery) []ptyevent.LearnedShape
	// AttachFixture records a regression fixture id without counting a sighting.
	AttachFixture(fp, fixtureID string) (ptyevent.LearnedShape, error)
}

// RouteRequest is the full context required to route and execute a decision.
type RouteRequest struct {
	SessionID       string
	AgentID         string
	Name            string
	RunID           string
	IsAutopilot     bool
	IsPipeline      bool
	Workdir         string
	BackendID       string
	CLIVersion      string
	Backend         agentbackend.Backend
	Posture         ptyevent.Posture
	AllowlistPolicy approval.Policy
	Act             bool

	Screen     string
	LivePrompt *ptyevent.Prompt

	// Strategic / Brain Consult context:
	PlanGoal            string
	ActiveTask          string
	Constraints         string
	TranscriptSummary   string
	GitState            string
	HistoricalDecisions []string

	// Stream and input routing:
	Stream      Stream
	InputWriter InputWriter
	HumanSeq    uint64
	LiveState   ptyevent.State

	// DryRun runs the whole decision (classification, posture, guards, live
	// revalidation) but never writes input. Shadow mode uses it to compare the
	// router with the legacy poller without acting.
	DryRun bool
}

// RouteResult describes the complete outcome of the decision router.
type RouteResult struct {
	Prompt             *ptyevent.Prompt
	Decision           ptyevent.Decision
	DecisionEvent      *ptyevent.DecisionEvent
	Delivered          bool
	DeliveredBytes     []byte
	RevalidationCode   string
	ArbitrationVerdict ptyevent.Verdict
	Downgraded         bool
	DowngradeReason    string
	BrainConsultResult *brainconsult.Result
}

// Config tunes the decision router behavior.
type Config struct {
	MaxDecisionAge time.Duration
	Ownership      ptyevent.OwnershipConfig
	ShapePolicy    ptyevent.ShapePolicy
	BrainTimeout   time.Duration
}

// DefaultConfig provides standard contract defaults.
func DefaultConfig() Config {
	return Config{
		MaxDecisionAge: ptyevent.DefaultMaxDecisionAge,
		Ownership:      ptyevent.DefaultOwnership,
		ShapePolicy:    ptyevent.DefaultShapePolicy,
		BrainTimeout:   5 * time.Minute,
	}
}
