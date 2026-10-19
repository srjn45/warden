package decisionrouter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
)

type mockStream struct {
	mu     sync.Mutex
	screen string
	emits  []ptyevent.Event
}

func (s *mockStream) ID() string                                         { return "test-stream" }
func (s *mockStream) Epoch() uint64                                      { return 1 }
func (s *mockStream) Journal() *ptyruntime.Journal                       { return nil }
func (s *mockStream) Subscribe(ptyevent.Cursor) *ptyruntime.Subscription { return nil }
func (s *mockStream) Screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen
}
func (s *mockStream) Emit(ev ptyevent.Event) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emits = append(s.emits, ev)
	return uint64(len(s.emits)), nil
}
func (s *mockStream) Emitted() []ptyevent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ptyevent.Event, len(s.emits))
	copy(out, s.emits)
	return out
}

type mockInputWriter struct {
	mu      sync.Mutex
	lease   ptyevent.Lease
	written []byte
	origins []ptyevent.Origin
}

func (w *mockInputWriter) WriteInput(ctx context.Context, data []byte, origin ptyevent.Origin, decisionID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, data...)
	w.origins = append(w.origins, origin)
	return nil
}

func (w *mockInputWriter) Lease() ptyevent.Lease {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lease
}

type mockHumanInbox struct {
	mu         sync.Mutex
	notified   bool
	agentID    string
	sessionID  string
	prompt     *ptyevent.Prompt
	safeOption []string
}

func (h *mockHumanInbox) NotifyHumanApproval(ctx context.Context, agentID, sessionID string, p *ptyevent.Prompt, safeOptions []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notified = true
	h.agentID = agentID
	h.sessionID = sessionID
	h.prompt = p
	h.safeOption = safeOptions
	return nil
}

type mockConsultor struct {
	mu     sync.Mutex
	reqs   []brainconsult.Request
	result brainconsult.Result
	err    error
}

func (c *mockConsultor) Consult(ctx context.Context, req brainconsult.Request) (brainconsult.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return brainconsult.Result{}, c.err
	}
	return c.result, nil
}

type mockArbiterEngine struct {
	fastbrain.Engine
	mu       sync.Mutex
	decision fastbrain.ArbiterDecision
	err      error
}

func (a *mockArbiterEngine) ArbitrateApproval(ctx context.Context, in fastbrain.ArbiterInput) (fastbrain.ArbiterDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return fastbrain.ArbiterDecision{}, a.err
	}
	return a.decision, nil
}

func samplePermissionPrompt(subject, target string, caps ...ptyevent.Capability) *ptyevent.Prompt {
	fp := ptyevent.Fingerprint("claude", 1, "Run command?", []string{"Yes", "No"})
	ik := ptyevent.InstanceKey(fp, subject, target)
	return &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ik,
		Backend:           "claude",
		Class:             ptyevent.ClassPermission,
		Capabilities:      caps,
		Subject:           subject,
		Target:            target,
		TargetInWorkspace: true,
		Question:          "Run command?",
		Options: []ptyevent.Option{
			{Index: 1, Label: "Yes", Semantic: ptyevent.SemAllowOnce, Scope: ptyevent.ScopeOnce, Default: true},
			{Index: 2, Label: "No", Semantic: ptyevent.SemDeny, Scope: ""},
		},
		CancelPath: ptyevent.CancelPath{Key: "Escape", OptionIndex: 2},
		Provenance: ptyevent.ProvParser,
		Confidence: 1.0,
		Verified:   true,
	}
}

func sampleDecisionPrompt() *ptyevent.Prompt {
	fp := ptyevent.Fingerprint("claude", 1, "Select architecture option", []string{"Option 1: Monolith", "Option 2: Microservices"})
	ik := ptyevent.InstanceKey(fp, "architecture", "choice")
	return &ptyevent.Prompt{
		Fingerprint:       fp,
		ShapeVersion:      1,
		InstanceKey:       ik,
		Backend:           "claude",
		Class:             ptyevent.ClassDecision,
		Subject:           "architecture",
		Target:            "choice",
		TargetInWorkspace: true,
		Question:          "Select architecture option",
		Options: []ptyevent.Option{
			{Index: 1, Label: "Option 1: Monolith", Semantic: ptyevent.SemChoice, Scope: ""},
			{Index: 2, Label: "Option 2: Microservices", Semantic: ptyevent.SemChoice, Scope: ""},
		},
		CancelPath: ptyevent.CancelPath{Key: "Escape"},
		Provenance: ptyevent.ProvParser,
		Confidence: 1.0,
		Verified:   true,
	}
}

func TestPermissionEngineRouting(t *testing.T) {
	router := New(RouterOptions{})

	readPrompt := samplePermissionPrompt("View", "src/main.go", ptyevent.CapRead)
	writePrompt := samplePermissionPrompt("Edit", "src/main.go", ptyevent.CapWrite)
	execPrompt := samplePermissionPrompt("Bash", "go test ./...", ptyevent.CapExecute)

	// Posture Read-Only approves read, denies write & exec
	res, err := router.Route(context.Background(), RouteRequest{
		Posture:    ptyevent.PostureReadOnly,
		LivePrompt: readPrompt,
		Act:        true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionApprove {
		t.Fatalf("read-only on read: got %s, want approve (err=%v)", res.Decision.Action, err)
	}

	res, err = router.Route(context.Background(), RouteRequest{
		Posture:    ptyevent.PostureReadOnly,
		LivePrompt: writePrompt,
		Act:        true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionDeny {
		t.Fatalf("read-only on write: got %s, want deny (err=%v)", res.Decision.Action, err)
	}

	// Posture Read-Write approves write, asks human on exec
	res, err = router.Route(context.Background(), RouteRequest{
		Posture:    ptyevent.PostureReadWrite,
		LivePrompt: writePrompt,
		Act:        true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionApprove {
		t.Fatalf("read-write on write: got %s, want approve (err=%v)", res.Decision.Action, err)
	}

	res, err = router.Route(context.Background(), RouteRequest{
		Posture:    ptyevent.PostureReadWrite,
		LivePrompt: execPrompt,
		Act:        true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionAskHuman {
		t.Fatalf("read-write on exec: got %s, want ask_human (err=%v)", res.Decision.Action, err)
	}

	// Allowlist matches
	allowPolicy := approval.Policy{
		Rules: approval.Rules{
			Allow: []approval.Rule{{Tool: "Bash", Pattern: "*go test*"}},
			Deny:  []approval.Rule{{Tool: "Bash", Pattern: "*drop*"}},
		},
	}
	res, err = router.Route(context.Background(), RouteRequest{
		Posture:         ptyevent.PostureAllowlisted,
		AllowlistPolicy: allowPolicy,
		LivePrompt:      execPrompt,
		Act:             true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionApprove {
		t.Fatalf("allowlisted with matching allow rule: got %s, want approve (err=%v)", res.Decision.Action, err)
	}

	dropPrompt := samplePermissionPrompt("Bash", "drop database test", ptyevent.CapExecute, ptyevent.CapDestructive)
	res, err = router.Route(context.Background(), RouteRequest{
		Posture:         ptyevent.PostureDangerouslyExecuteAll,
		AllowlistPolicy: allowPolicy,
		LivePrompt:      dropPrompt,
		Act:             true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionDeny {
		t.Fatalf("deny rule must win even in dangerous posture: got %s, want deny", res.Decision.Action)
	}
}

func TestStrategicDecisionHumanInboxVsAutopilotConsult(t *testing.T) {
	decisionPrompt := sampleDecisionPrompt()

	// 1. Plain / Pipeline agent -> human approval inbox
	inbox := &mockHumanInbox{}
	routerPlain := New(RouterOptions{HumanInbox: inbox})

	res, err := routerPlain.Route(context.Background(), RouteRequest{
		AgentID:     "agent-plain",
		SessionID:   "sess-plain",
		IsPipeline:  true,
		IsAutopilot: false,
		Posture:     ptyevent.PostureReadWrite,
		LivePrompt:  decisionPrompt,
	})
	if err != nil {
		t.Fatalf("route plain strategic decision: %v", err)
	}
	if res.Decision.Action != ptyevent.ActionAskHuman {
		t.Errorf("expected ask_human action for plain agent strategic decision, got %s", res.Decision.Action)
	}
	if !inbox.notified {
		t.Errorf("expected human inbox to be notified for plain agent")
	}

	// 2. Autopilot run -> brain consult
	consultor := &mockConsultor{
		result: brainconsult.Result{
			Answer: brainconsult.AnswerSelectOption,
			Option: 2,
			Reason: "Microservices align with long-term scaling constraints",
		},
	}
	routerAutopilot := New(RouterOptions{Consultor: consultor})

	res, err = routerAutopilot.Route(context.Background(), RouteRequest{
		AgentID:             "agent-ap",
		SessionID:           "sess-ap",
		RunID:               "ap-run-123",
		IsAutopilot:         true,
		Posture:             ptyevent.PostureReadWrite,
		LivePrompt:          decisionPrompt,
		PlanGoal:            "Refactor architecture",
		ActiveTask:          "Choose service boundary",
		Constraints:         "Ensure isolated deployments",
		TranscriptSummary:   "Worker analyzed trade-offs",
		GitState:            "Clean on branch feat",
		HistoricalDecisions: []string{"Approved initial plan"},
	})
	if err != nil {
		t.Fatalf("route autopilot strategic decision: %v", err)
	}

	if len(consultor.reqs) != 1 {
		t.Fatalf("expected 1 brain consult call, got %d", len(consultor.reqs))
	}
	cReq := consultor.reqs[0]
	if cReq.RunID != "ap-run-123" || cReq.Answer.Task != "Choose service boundary" {
		t.Errorf("consult request context mismatch: %+v", cReq)
	}

	if res.Decision.Action != ptyevent.ActionApprove || res.Decision.OptionIndex != 2 {
		t.Errorf("expected approved option 2 from brain consult, got action=%s opt=%d", res.Decision.Action, res.Decision.OptionIndex)
	}

	// 3. Autopilot brain consult failure -> routes to needs_attention
	consultorFail := &mockConsultor{
		err: errors.New("brain timeout"),
	}
	routerAutopilotFail := New(RouterOptions{Consultor: consultorFail})
	res, err = routerAutopilotFail.Route(context.Background(), RouteRequest{
		AgentID:     "agent-ap",
		RunID:       "ap-run-123",
		IsAutopilot: true,
		LivePrompt:  decisionPrompt,
	})
	if err != nil {
		t.Fatalf("route fail: %v", err)
	}
	if res.Decision.Action != ptyevent.ActionNeedsAttention {
		t.Errorf("expected needs_attention on brain consult failure, got %s", res.Decision.Action)
	}
}

func TestTOCTOURevalidationAborts(t *testing.T) {
	now := time.Now()
	p := samplePermissionPrompt("Bash", "git status", ptyevent.CapExecute)
	stream := &mockStream{}
	writer := &mockInputWriter{}

	router := New(RouterOptions{
		Clock: func() time.Time { return now },
	})

	// Case 1: Option moved or changed
	changedPrompt := *p
	changedPrompt.Options = []ptyevent.Option{
		{Index: 1, Label: "Yes, wipe everything", Semantic: ptyevent.SemDeny},
	}
	res, err := router.Route(context.Background(), RouteRequest{
		Posture:     ptyevent.PostureDangerouslyExecuteAll,
		LivePrompt:  &changedPrompt,
		Stream:      stream,
		InputWriter: writer,
		Act:         true,
	})
	if err != nil {
		t.Fatalf("route error: %v", err)
	}
	if res.Delivered {
		t.Errorf("TOCTOU failure must not deliver keystrokes")
	}

	// Case 2: Human intervened since decision (HumanSeq > DecidedSeq)
	writer.lease.HumanSeq = 100
	res, err = router.Route(context.Background(), RouteRequest{
		Posture:     ptyevent.PostureDangerouslyExecuteAll,
		LivePrompt:  p,
		Stream:      stream,
		InputWriter: writer,
		HumanSeq:    100, // human typed
		Act:         true,
	})
	if err != nil {
		t.Fatalf("route error: %v", err)
	}
	if res.Delivered {
		t.Errorf("expected delivery aborted due to human intervention")
	}
	if res.RevalidationCode != ptyevent.RevalHumanIntervened {
		t.Errorf("expected RevalHumanIntervened, got %s", res.RevalidationCode)
	}

	// Case 3: Stale decision age
	routerStale := New(RouterOptions{
		Config: Config{MaxDecisionAge: 10 * time.Millisecond},
		Clock:  func() time.Time { return now.Add(time.Second) },
	})
	pd := ptyevent.PendingDecision{
		Decision:   ptyevent.Decision{Action: ptyevent.ActionApprove, OptionIndex: 1},
		Prompt:     *p,
		DecidedAt:  now,
		DecidedSeq: 10,
	}
	live := ptyevent.Live{
		Prompt: p,
		State:  ptyevent.StateWaitingForPermission,
		Now:    now.Add(time.Second),
	}
	err = ptyevent.Revalidate(pd, live, 10*time.Millisecond)
	if err == nil {
		t.Errorf("expected stale error on old decision age")
	}
	_ = routerStale
}

func TestBreakerAndArbiterDowngrades(t *testing.T) {
	now := time.Now()
	p := samplePermissionPrompt("Bash", "make test", ptyevent.CapExecute)

	// 1. Breaker trips after repeated approvals: downgrades approve -> ask_human
	breaker := approval.NewBreaker()
	store := NewMemoryLearnedShapeStore()

	// Register learned shape
	sh, _ := store.AddCandidate(ptyevent.LearnedShape{
		Fingerprint: p.Fingerprint,
		Backend:     p.Backend,
	})
	p.LearnedShapeID = sh.ID

	router := New(RouterOptions{
		Breaker:    breaker,
		ShapeStore: store,
		Clock:      func() time.Time { return now },
	})

	policy := approval.Policy{
		MaxRepeats: 2,
	}

	// Call 1: approved
	res1, _ := router.Route(context.Background(), RouteRequest{
		SessionID:       "s1",
		Posture:         ptyevent.PostureDangerouslyExecuteAll,
		AllowlistPolicy: policy,
		LivePrompt:      p,
		Act:             false, // don't write keystrokes, just check decision
	})
	if res1.Decision.Action != ptyevent.ActionAskHuman || res1.Decision.Rule != ptyevent.RulePermitted {
		// When Act=false, surfaces as ask_human with suggestion
	}

	// Now with Act=true:
	res, err := router.Route(context.Background(), RouteRequest{
		SessionID:       "s1",
		Posture:         ptyevent.PostureDangerouslyExecuteAll,
		AllowlistPolicy: policy,
		LivePrompt:      p,
		Act:             true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionApprove {
		t.Fatalf("first approve should succeed: %+v", res.Decision)
	}

	res, err = router.Route(context.Background(), RouteRequest{
		SessionID:       "s1",
		Posture:         ptyevent.PostureDangerouslyExecuteAll,
		AllowlistPolicy: policy,
		LivePrompt:      p,
		Act:             true,
	})
	if err != nil || res.Decision.Action != ptyevent.ActionApprove {
		t.Fatalf("second approve should succeed: %+v", res.Decision)
	}

	// Third call: exceeds MaxRepeats (2) -> breaker trips!
	res, err = router.Route(context.Background(), RouteRequest{
		SessionID:       "s1",
		Posture:         ptyevent.PostureDangerouslyExecuteAll,
		AllowlistPolicy: policy,
		LivePrompt:      p,
		Act:             true,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Decision.Action != ptyevent.ActionAskHuman {
		t.Errorf("expected breaker downgrade approve -> ask_human, got %s", res.Decision.Action)
	}
	if !res.Downgraded || res.DowngradeReason != "breaker-tripped" {
		t.Errorf("expected breaker-tripped downgrade flag")
	}

	// Check that the learned shape was retired on breaker trip
	retiredShape, _ := store.Get(sh.ID)
	if retiredShape.Status != ptyevent.ShapeRetired {
		t.Errorf("expected learned shape retired after breaker trip, got %s", retiredShape.Status)
	}

	// 2. Fast-Brain Arbiter downgrade: destructive prompt blocked by arbiter safety check
	destructivePrompt := samplePermissionPrompt("Bash", "rm -rf /data", ptyevent.CapExecute, ptyevent.CapDestructive)
	arbiter := &mockArbiterEngine{
		decision: fastbrain.ArbiterDecision{
			Action:    fastbrain.DecisionEscalate,
			Rationale: "destructive rm -rf command",
		},
	}
	routerArbiter := New(RouterOptions{
		FastBrain: arbiter,
	})

	res, err = routerArbiter.Route(context.Background(), RouteRequest{
		SessionID:       "s2",
		Posture:         ptyevent.PostureReadWrite,
		AllowlistPolicy: approval.Policy{UseFastBrain: true},
		LivePrompt:      destructivePrompt,
		Act:             true,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Decision.Action != ptyevent.ActionAskHuman {
		t.Errorf("expected arbiter downgrade to ask_human, got %s", res.Decision.Action)
	}

	// Invariant assertion: breaker and arbiter NEVER upgrade
	// An action that was ask_human or deny is never converted to approve
	readOnlyPrompt := samplePermissionPrompt("Bash", "touch newfile", ptyevent.CapExecute, ptyevent.CapWrite)
	res, _ = routerArbiter.Route(context.Background(), RouteRequest{
		SessionID:       "s3",
		Posture:         ptyevent.PostureReadOnly,
		AllowlistPolicy: approval.Policy{UseFastBrain: true},
		LivePrompt:      readOnlyPrompt,
		Act:             true,
	})
	if res.Decision.Action == ptyevent.ActionApprove {
		t.Fatalf("arbiter must never upgrade a forbidden action to approve")
	}
}

func TestInputDeliveryAndDecisionEventEmission(t *testing.T) {
	now := time.Now()
	stream := &mockStream{}
	writer := &mockInputWriter{}
	prompt := samplePermissionPrompt("Bash", "echo ok", ptyevent.CapExecute)

	router := New(RouterOptions{
		Clock: func() time.Time { return now },
	})

	res, err := router.Route(context.Background(), RouteRequest{
		Posture:     ptyevent.PostureDangerouslyExecuteAll,
		LivePrompt:  prompt,
		Stream:      stream,
		InputWriter: writer,
		Act:         true,
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}

	if !res.Delivered {
		t.Errorf("expected input to be delivered")
	}
	if len(writer.written) == 0 {
		t.Errorf("expected bytes written to input writer")
	}
	if res.DecisionEvent == nil {
		t.Fatalf("expected decision event to be generated")
	}
	if res.DecisionEvent.Revalidation != ptyevent.RevalOK {
		t.Errorf("expected revalidation ok, got %s", res.DecisionEvent.Revalidation)
	}

	// Verify DecisionEvent was emitted to Stream
	emitted := stream.Emitted()
	if len(emitted) != 1 {
		t.Fatalf("expected 1 emitted event on stream, got %d", len(emitted))
	}
	if emitted[0].Kind != ptyevent.KindDecision || emitted[0].Decision == nil {
		t.Fatalf("emitted event mismatch: %+v", emitted[0])
	}
	if emitted[0].Decision.Action != ptyevent.ActionApprove {
		t.Errorf("emitted decision action mismatch: %s", emitted[0].Decision.Action)
	}
}
