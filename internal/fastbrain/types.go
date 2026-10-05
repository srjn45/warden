// Package fastbrain is the universal "Fast-Brain Gateway": a single port
// (Engine.Decide) through which every cheap, latency-bounded micro-decision
// (approval arbitration, agent naming, future diagnosis/summaries) is routed to
// a fast- or thinking-tier model. The package is store- and backend-free; the
// daemon wires concrete Runners.
package fastbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// DecisionKind names the micro-decision being requested.
type DecisionKind string

const (
	KindArbitrateApproval DecisionKind = "arbitrate_approval"
	KindResolveAgentName  DecisionKind = "resolve_agent_name"
	KindDiagnoseFailure   DecisionKind = "diagnose_failure"
	// KindDiagnoseStall picks the recovery action for a stalled manager/worker.
	KindDiagnoseStall DecisionKind = "diagnose_stall"
	// KindClassifyCIFailure rates a red CI log as flaky_or_infra or real.
	KindClassifyCIFailure DecisionKind = "classify_ci_failure"
	// KindSummarizeActivity is reserved for activity summaries (prompt+parser
	// exist; callers land separately).
	KindSummarizeActivity DecisionKind = "summarize_activity"
	// KindClassifyTask classifies a prompt into a task type.
	KindClassifyTask DecisionKind = "classify_task"
	// KindRouteTier rates how hard a spawn prompt is and suggests a model tier
	// (tier-1/tier-2/tier-3) for the quota-balanced resolver.
	KindRouteTier DecisionKind = "route_tier"
	// KindSummarizeCheck condenses oversized test/linter failures.
	KindSummarizeCheck DecisionKind = "summarize_check"
	// KindCommitMessage drafts a conventional commit message from a diff.
	KindCommitMessage DecisionKind = "commit_message"
	// KindPRSummary drafts a pull-request title and body from the task, diff
	// stat and commit subjects.
	KindPRSummary DecisionKind = "pr_summary"
	// KindCurateExtract extracts durable facts as bullet entries.
	KindCurateExtract DecisionKind = "curate_extract"
	// KindReplTurn plans tool calls or prose for a REPL turn.
	KindReplTurn DecisionKind = "repl_turn"
)

// Tier selects the model class (and therefore the timeout budget).
type Tier string

const (
	TierFast     Tier = "fast"
	TierThinking Tier = "thinking"
)

// Request is the input envelope for Engine.Decide.
type Request struct {
	Kind     DecisionKind
	Tier     Tier
	Prompt   string            // full prompt sent to the runner
	Input    any               // optional structured input (caller context; not sent)
	Metadata map[string]string // free-form, e.g. agent id; never logged verbatim
	// Timeout optionally lowers the tier budget. It can never exceed the tier
	// maximum (1.5s fast / 10s thinking).
	Timeout time.Duration
}

// Status describes how a Decide call ended.
type Status string

const (
	StatusOK          Status = "ok"
	StatusTimeout     Status = "timeout"
	StatusCanceled    Status = "canceled"
	StatusRunnerError Status = "runner_error"
	StatusNoRunner    Status = "no_runner"
	StatusInvalidJSON Status = "invalid_json"
)

// Output carries the model reply: the raw text and, when it contained a JSON
// object, the sanitized object.
type Output struct {
	Raw    string
	Parsed json.RawMessage // sanitized JSON object; nil unless Status == StatusOK
}

// Response is the output envelope. Callers must check Status (or OK) — any
// non-OK status means "fail open": escalate or fall back to the existing path.
type Response struct {
	Kind       DecisionKind
	Tier       Tier
	Output     Output
	Confidence float64 // from the model's optional "confidence" field; 0 if absent
	Rationale  string  // from the model's optional "rationale" field
	Status     Status
	Error      string // human-readable cause when Status != StatusOK
	Duration   time.Duration
}

// OK reports whether the decision completed with valid JSON output.
func (r Response) OK() bool { return r.Status == StatusOK }

// ErrInvalidRequest marks a programmer mistake in the Request.
var ErrInvalidRequest = errors.New("fastbrain: invalid request")

// Engine is the single evaluation path for all fast-brain decisions.
type Engine interface {
	Decide(ctx context.Context, req Request) (Response, error)
	// ArbitrateApproval decides a pending approval/question prompt. It is
	// built on Decide and fails open to DecisionEscalate.
	ArbitrateApproval(ctx context.Context, in ArbiterInput) (ArbiterDecision, error)
	// DiagnoseFailure classifies an agent crash. Built on Decide; fails open to
	// a deterministic heuristic classifier.
	DiagnoseFailure(ctx context.Context, in CrashInput) (CrashDiagnosis, error)
}

// ErrNoJSON is returned by SanitizeJSON when no JSON object can be found.
var ErrNoJSON = errors.New("fastbrain: no JSON object in model output")

// SanitizeJSON strips markdown fences and conversational preamble/postamble and
// returns the first complete, valid JSON object found in s.
func SanitizeJSON(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrNoJSON
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return raw, nil
		}
	}
	return nil, ErrNoJSON
}

// extractMeta pulls optional confidence/rationale from a parsed object.
func extractMeta(obj json.RawMessage) (float64, string) {
	var m struct {
		Confidence float64 `json:"confidence"`
		Rationale  string  `json:"rationale"`
	}
	_ = json.Unmarshal(obj, &m)
	return m.Confidence, m.Rationale
}
