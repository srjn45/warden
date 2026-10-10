package fastbrain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// StallAction is the closed set of recovery actions a stall diagnosis may pick.
type StallAction string

const (
	ActionWait            StallAction = "wait"
	ActionNudge           StallAction = "nudge"
	ActionResolvePrompt   StallAction = "resolve_prompt"
	ActionResumeRateLimit StallAction = "resume_rate_limit"
	ActionRedeliverPrompt StallAction = "redeliver_prompt"
	ActionRestart         StallAction = "restart"
	ActionRotate          StallAction = "rotate"
	ActionCallResolver    StallAction = "call_resolver"
	// ActionMechanical is never a valid model answer: it tells the caller to
	// run its own mechanical ladder (heuristics found nothing, or fail-open).
	ActionMechanical StallAction = "mechanical"
)

// MaxNudgeChars caps the nudge text of a diagnosis.
const MaxNudgeChars = 600

// StallLowConfidence is the confidence below which the fast pass is re-asked
// on the thinking tier.
const StallLowConfidence = 0.6

// StallPaneLines is how many trailing pane lines enter the prompt.
const StallPaneLines = 60

// managerActions is the full action set; workerActions the smaller one for
// workers (no restart/rotate/call_resolver: those belong to the manager ladder).
var (
	managerActions = []StallAction{ActionWait, ActionNudge, ActionResolvePrompt, ActionResumeRateLimit,
		ActionRedeliverPrompt, ActionRestart, ActionRotate, ActionCallResolver}
	workerActions = []StallAction{ActionWait, ActionNudge, ActionResolvePrompt, ActionResumeRateLimit,
		ActionRedeliverPrompt}
)

// AllowedStallActions returns the action set for a manager or worker.
func AllowedStallActions(worker bool) []StallAction {
	if worker {
		return append([]StallAction(nil), workerActions...)
	}
	return append([]StallAction(nil), managerActions...)
}

func actionAllowed(a StallAction, worker bool) bool {
	for _, x := range AllowedStallActions(worker) {
		if x == a {
			return true
		}
	}
	return false
}

// StallInput describes a stalled agent. Pane/log text is sanitized before it
// enters a prompt.
type StallInput struct {
	AgentID string
	Worker  bool   // true = worker action set
	Header  string // run id, heal stage, minutes since activity/progress, ...
	Facts   string // backend, tier, status, context level, ...
	Pane    string // pane tail; only the last StallPaneLines lines are used
	// Deterministic signals.
	RateLimited        bool
	PendingApproval    bool
	ActivitySinceSpawn bool // any output/activity observed since spawn
}

// Fail-open reasons (StallDiagnosis.FailOpen, CIClassification.FailOpen).
const (
	FailOpenNoRunner      = "no_runner"
	FailOpenTimeout       = "timeout"
	FailOpenCanceled      = "canceled"
	FailOpenRunnerError   = "runner_error"
	FailOpenInvalidJSON   = "invalid_json"
	FailOpenInvalidAction = "invalid_action"
	FailOpenInvalidOutput = "invalid_output"
)

func failOpenReason(r Response) string {
	switch r.Status {
	case StatusTimeout:
		return FailOpenTimeout
	case StatusCanceled:
		return FailOpenCanceled
	case StatusNoRunner:
		return FailOpenNoRunner
	case StatusInvalidJSON:
		return FailOpenInvalidJSON
	default:
		return FailOpenRunnerError
	}
}

// StallDiagnosis is the triage result. Action == ActionMechanical means the
// caller runs its own ladder; FailOpen then says why (empty = heuristics
// simply found nothing and no model was available/asked).
type StallDiagnosis struct {
	Action     StallAction
	Text       string // nudge text only; sanitized, <= MaxNudgeChars
	Confidence float64
	Rationale  string
	Tier       Tier   // tier of the answering model; "" for heuristics/fail-open
	Source     string // "heuristic", "model" or "failopen"
	FailOpen   string
}

// HeuristicStall applies the deterministic, model-free rules. It returns
// ActionMechanical when none applies.
func HeuristicStall(in StallInput) StallDiagnosis {
	h := func(a StallAction, why string) StallDiagnosis {
		return StallDiagnosis{Action: a, Confidence: 1, Rationale: why, Source: "heuristic"}
	}
	switch {
	case in.RateLimited:
		return h(ActionResumeRateLimit, "rate-limit flag set")
	case in.PendingApproval:
		return h(ActionResolvePrompt, "pending approval")
	case strings.TrimSpace(in.Pane) == "" && !in.ActivitySinceSpawn:
		return h(ActionRedeliverPrompt, "empty pane, no activity since spawn")
	}
	return StallDiagnosis{Action: ActionMechanical, Source: "heuristic", Rationale: "no heuristic matched"}
}

const stallSystemFmt = `You diagnose a stalled coding agent and pick ONE recovery action. Allowed actions: %s. "wait": healthy but slow. "nudge": wake it with short, specific text (max 600 chars). "resolve_prompt": an unanswered prompt is on screen. "resume_rate_limit": parked at a usage-limit menu/banner. "redeliver_prompt": it never received or lost its brief.%s Reply with ONLY this JSON, no prose: {"action":"<one allowed>","text":"<nudge text, only for nudge>","confidence":0.0-1.0,"rationale":"<short>"}`

const stallManagerExtra = ` "restart": restart in place with fresh context. "rotate": switch to another backend. "call_resolver": a blocker it cannot clear itself.`

func stallPrompt(in StallInput) string {
	acts := make([]string, 0, 8)
	for _, a := range AllowedStallActions(in.Worker) {
		acts = append(acts, string(a))
	}
	extra := stallManagerExtra
	if in.Worker {
		extra = ""
	}
	return fmt.Sprintf(stallSystemFmt, strings.Join(acts, ", "), extra) +
		fmt.Sprintf("\n\nHeader: %s\nFacts: %s\nPane tail:\n%s\n",
			clip(Sanitize(in.Header)), clip(Sanitize(in.Facts)), clip(Sanitize(tailLines(in.Pane, StallPaneLines))))
}

// ParseStallDiagnosis strictly parses a model JSON object: unknown (or, for
// workers, disallowed) actions, a nudge without text and out-of-range
// confidence are rejected. Nudge text is sanitized and capped.
func ParseStallDiagnosis(obj json.RawMessage, worker bool) (StallDiagnosis, error) {
	var m struct {
		Action     string   `json:"action"`
		Text       string   `json:"text"`
		Confidence *float64 `json:"confidence"`
		Rationale  string   `json:"rationale"`
	}
	if err := json.Unmarshal(obj, &m); err != nil {
		return StallDiagnosis{}, fmt.Errorf("%s: %v", FailOpenInvalidJSON, err)
	}
	a := StallAction(strings.TrimSpace(m.Action))
	if !actionAllowed(a, worker) {
		return StallDiagnosis{}, fmt.Errorf("%s: %q", FailOpenInvalidAction, m.Action)
	}
	var conf float64
	if m.Confidence != nil {
		conf = *m.Confidence
		if math.IsNaN(conf) || conf < 0 || conf > 1 {
			return StallDiagnosis{}, fmt.Errorf("%s: confidence %v out of range", FailOpenInvalidOutput, conf)
		}
	}
	d := StallDiagnosis{Action: a, Confidence: conf, Rationale: Sanitize(m.Rationale), Source: "model"}
	if a == ActionNudge {
		t := strings.TrimSpace(Sanitize(m.Text))
		if t == "" {
			return StallDiagnosis{}, fmt.Errorf("%s: nudge without text", FailOpenInvalidOutput)
		}
		if r := []rune(t); len(r) > MaxNudgeChars {
			t = string(r[:MaxNudgeChars])
		}
		d.Text = t
	}
	return d, nil
}

func stallFailOpen(reason string) StallDiagnosis {
	return StallDiagnosis{Action: ActionMechanical, Source: "failopen", FailOpen: reason, Rationale: "fail open: " + reason}
}

// askStall runs one tier; ok=false carries the fail-open reason.
func askStall(ctx context.Context, d decider, in StallInput, tier Tier) (StallDiagnosis, string) {
	resp, err := d.Decide(ctx, Request{
		Kind: KindDiagnoseStall, Tier: tier, Prompt: stallPrompt(in),
		Metadata: map[string]string{"agent_id": in.AgentID},
	})
	if err != nil {
		return StallDiagnosis{}, FailOpenRunnerError
	}
	if !resp.OK() {
		return StallDiagnosis{}, failOpenReason(resp)
	}
	out, err := ParseStallDiagnosis(resp.Output.Parsed, in.Worker)
	if err != nil {
		reason := FailOpenInvalidOutput
		if strings.HasPrefix(err.Error(), FailOpenInvalidAction) {
			reason = FailOpenInvalidAction
		} else if strings.HasPrefix(err.Error(), FailOpenInvalidJSON) {
			reason = FailOpenInvalidJSON
		}
		return StallDiagnosis{}, reason
	}
	out.Tier = tier
	return out, ""
}

// DiagnoseStall runs the heuristics, then the fast tier, then the thinking
// tier only when the fast answer is low confidence. It never errors for model
// problems: those fail open to ActionMechanical.
func DiagnoseStall(ctx context.Context, e Engine, in StallInput) StallDiagnosis {
	if h := HeuristicStall(in); h.Action != ActionMechanical {
		return h
	}
	if e == nil {
		return stallFailOpen(FailOpenNoRunner)
	}
	return diagnoseStall(ctx, e, in)
}

func diagnoseStall(ctx context.Context, d decider, in StallInput) StallDiagnosis {
	out, reason := askStall(ctx, d, in, TierFast)
	if reason != "" {
		return stallFailOpen(reason)
	}
	if out.Confidence >= StallLowConfidence {
		return out
	}
	deep, reason := askStall(ctx, d, in, TierThinking)
	if reason != "" {
		return stallFailOpen(reason)
	}
	return deep
}

// CIClass is the CI-failure verdict.
type CIClass string

const (
	CIFlakyOrInfra CIClass = "flaky_or_infra"
	CIReal         CIClass = "real"
)

// CIInput is a red check's log.
type CIInput struct {
	AgentID string
	Check   string // check/job name
	Log     string // log tail; only the last ExcerptLines are used
}

// CIClassification is the verdict. Fail-open yields CIReal (never ignore a
// failure we could not judge) with Confidence 0.
type CIClassification struct {
	Class      CIClass
	Confidence float64
	Rationale  string
	Source     string // "model" or "failopen"
	FailOpen   string
}

const ciSystem = `Classify a failed CI check. "flaky_or_infra": infrastructure or flakiness unrelated to the code change (network timeout, runner shutdown, 429/5xx from a registry, cache or download failure, known-flaky test passing on rerun). "real": a genuine compile, lint or test failure caused by the code. Reply with ONLY this JSON, no prose: {"class":"flaky_or_infra|real","confidence":0.0-1.0,"rationale":"<short>"}`

func ciPrompt(in CIInput) string {
	return fmt.Sprintf("%s\n\nCheck: %s\nLog tail:\n%s\n", ciSystem, Sanitize(in.Check), clip(Sanitize(tailLines(in.Log, ExcerptLines))))
}

// ParseCIClassification strictly parses a model reply.
func ParseCIClassification(obj json.RawMessage) (CIClassification, error) {
	var m struct {
		Class      string   `json:"class"`
		Confidence *float64 `json:"confidence"`
		Rationale  string   `json:"rationale"`
	}
	if err := json.Unmarshal(obj, &m); err != nil {
		return CIClassification{}, err
	}
	c := CIClass(strings.TrimSpace(m.Class))
	if c != CIFlakyOrInfra && c != CIReal {
		return CIClassification{}, fmt.Errorf("unknown class %q", m.Class)
	}
	var conf float64
	if m.Confidence != nil {
		conf = *m.Confidence
		if math.IsNaN(conf) || conf < 0 || conf > 1 {
			return CIClassification{}, fmt.Errorf("confidence %v out of range", conf)
		}
	}
	return CIClassification{Class: c, Confidence: conf, Rationale: Sanitize(m.Rationale), Source: "model"}, nil
}

// ClassifyCIFailure asks the fast tier. It fails open to CIReal.
func ClassifyCIFailure(ctx context.Context, e Engine, in CIInput) CIClassification {
	fo := func(reason string) CIClassification {
		return CIClassification{Class: CIReal, Source: "failopen", FailOpen: reason, Rationale: "fail open: " + reason}
	}
	if e == nil {
		return fo(FailOpenNoRunner)
	}
	return classifyCI(ctx, e, in, fo)
}

func classifyCI(ctx context.Context, d decider, in CIInput, fo func(string) CIClassification) CIClassification {
	resp, err := d.Decide(ctx, Request{
		Kind: KindClassifyCIFailure, Tier: TierFast, Prompt: ciPrompt(in),
		Metadata: AgentMeta(in.AgentID), Timeout: KindDeadline(KindClassifyCIFailure),
	})
	if err != nil {
		return fo(FailOpenRunnerError)
	}
	if !resp.OK() {
		return fo(failOpenReason(resp))
	}
	out, err := ParseCIClassification(resp.Output.Parsed)
	if err != nil {
		return fo(FailOpenInvalidOutput)
	}
	return out
}
