package fastbrain

// Per-kind caller contract (design: docs/specs/2026-10-10-fast-brain-decision-
// inventory-and-slos.md §3–§5). Callers use these helpers so that every Decide
// carries the identity the admission controller needs for per-agent fairness
// and a bounded deadline matching the kind's SLO class. Deadlines only ever
// LOWER the tier budget (Request.Timeout can never exceed the tier ceiling), so
// no tier timeout is enlarged.

import "time"

// Per-kind hard deadlines for callers whose decision is worthless once late.
const (
	// CallerDeadlineOperational bounds P3 kinds that have a deterministic
	// fallback (commit message, check summary, CI classification, PR summary).
	CallerDeadlineOperational = 10 * time.Second
	// CallerDeadlineBestEffort bounds P4 kinds (naming, classification,
	// curation, narration); they are shed rather than queued anyway.
	CallerDeadlineBestEffort = 8 * time.Second
	// ReplTurnDeadline bounds one P2 REPL turn (spec §4: 20 s, never above the
	// tier ceiling, which wins when lower).
	ReplTurnDeadline = 20 * time.Second
	// CallerDeadlineRouteTier is the spawn-path router's hard bound.
	CallerDeadlineRouteTier = 1500 * time.Millisecond
)

// KindDeadline returns the caller-side deadline for kind, or 0 when the tier
// ceiling is the right bound (P1/P2 kinds, which own their own escalation).
func KindDeadline(kind DecisionKind) time.Duration {
	switch kind {
	case KindRouteTier:
		return CallerDeadlineRouteTier
	case KindDiagnoseFailure, KindClassifyCIFailure, KindSummarizeCheck,
		KindCommitMessage, KindPRSummary:
		return CallerDeadlineOperational
	case KindClassifyTask, KindResolveAgentName, KindSummarizeActivity, KindCurateExtract:
		return CallerDeadlineBestEffort
	default:
		return 0
	}
}

// AgentMeta returns the request Metadata identifying the owning agent for
// per-agent fairness. An empty id yields nil (the call gets its own bucket).
func AgentMeta(agentID string) map[string]string {
	if agentID == "" {
		return nil
	}
	return map[string]string{"agent_id": agentID}
}

// NewRequest builds a Request for kind/tier/prompt carrying the kind's caller
// deadline and the owning agent's identity.
func NewRequest(kind DecisionKind, tier Tier, prompt, agentID string) Request {
	return Request{Kind: kind, Tier: tier, Prompt: prompt, Metadata: AgentMeta(agentID), Timeout: KindDeadline(kind)}
}
