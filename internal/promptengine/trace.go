package promptengine

import (
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
)

// attempt is the context of one router run, enough to write its trace record.
type attempt struct {
	session string
	ag      *agentstore.Agent
	req     decisionrouter.RouteRequest
	started time.Time
	prompt  *ptyevent.Prompt // the prompt routed (nil: nothing readable)
	// unreadable carries the reason when no usable prompt could be formed.
	unreadable string
}

// outcomeOf maps a router result to the trace's terminal outcome and the
// escalation reason (empty when nothing escalated).
func outcomeOf(res decisionrouter.RouteResult, err error, dry bool) (ptyobs.Outcome, string) {
	if err != nil {
		return ptyobs.OutcomeError, ""
	}
	dec := res.Decision
	switch {
	case res.RevalidationCode != "" && res.RevalidationCode != ptyevent.RevalOK:
		if res.ArbitrationVerdict != "" && string(res.ArbitrationVerdict) != "write" &&
			res.RevalidationCode == string(res.ArbitrationVerdict) {
			return ptyobs.OutcomeDeferred, "arbitration"
		}
		return ptyobs.OutcomeDeferred, "revalidation"
	case res.Downgraded:
		return ptyobs.OutcomeEscalated, downgradeReason(res.DowngradeReason)
	}
	switch dec.Action {
	case ptyevent.ActionApprove, ptyevent.ActionDeny:
		if dry {
			return ptyobs.OutcomeDryRun, ""
		}
		if res.Route == "brain" {
			return ptyobs.OutcomeRoutedBrain, ""
		}
		if dec.Action == ptyevent.ActionDeny {
			return ptyobs.OutcomeDenied, ""
		}
		return ptyobs.OutcomeApproved, ""
	case ptyevent.ActionAskHuman:
		if res.Route == "human" {
			return ptyobs.OutcomeEscalated, "strategic"
		}
		return ptyobs.OutcomeEscalated, "other"
	case ptyevent.ActionNeedsAttention:
		switch {
		case strings.HasPrefix(dec.Rule, "brain-consult"):
			return ptyobs.OutcomeAttention, "brain_failed"
		case res.Prompt != nil && !res.Prompt.Verified && res.Prompt.Provenance != ptyevent.ProvParser:
			return ptyobs.OutcomeAttention, "unsafe_format"
		case res.Prompt != nil && res.Prompt.Confidence < ptyevent.MinConfidenceParser:
			return ptyobs.OutcomeAttention, "low_confidence"
		}
		return ptyobs.OutcomeAttention, "ambiguous"
	}
	return ptyobs.OutcomeAttention, "ambiguous"
}

func downgradeReason(r string) string {
	switch {
	case r == "act-switch-disabled":
		return "act_disabled"
	case r == "breaker-tripped":
		return "breaker"
	case strings.HasPrefix(r, "arbiter"):
		return "arbiter"
	}
	return "other"
}

// routeOf names who owns the prompt after the router ran.
func routeOf(res decisionrouter.RouteResult, o ptyobs.Outcome) ptyobs.Route {
	switch {
	case res.Route == "brain":
		return ptyobs.RouteBrain
	case res.Route == "human", o == ptyobs.OutcomeEscalated:
		return ptyobs.RouteHuman
	case o == ptyobs.OutcomeAttention:
		return ptyobs.RouteNone
	}
	return ptyobs.RoutePosture
}

// observe writes the trace record and metrics for one finished attempt.
func (e *Engine) observe(a attempt, res decisionrouter.RouteResult, err error) {
	if e.cfg.Trace == nil && e.cfg.Metrics == nil {
		return
	}
	dry := a.req.DryRun
	outcome, esc := ptyobs.OutcomeAttention, "unrecognized"
	route := ptyobs.RouteNone
	if a.prompt != nil || res.Prompt != nil {
		outcome, esc = outcomeOf(res, err, dry)
		route = routeOf(res, outcome)
	} else if err != nil {
		outcome, esc = ptyobs.OutcomeError, ""
	}
	p := a.prompt
	if res.Prompt != nil {
		p = res.Prompt
	}
	latency := e.cfg.Clock().Sub(a.started)

	chosen := 0
	if outcome != ptyobs.OutcomeEscalated && outcome != ptyobs.OutcomeAttention && outcome != ptyobs.OutcomeDeferred &&
		outcome != ptyobs.OutcomeError {
		chosen = res.Decision.OptionIndex
	}
	rec := ptyobs.TraceRecord{
		Time:      e.cfg.Clock(),
		SessionID: a.session,
		AgentID:   a.req.AgentID,
		Adapter:   a.req.BackendID, CLIVersion: a.req.CLIVersion,
		Permission: ptyobs.Summarize(p, chosen),
		Posture:    string(a.req.Posture),
		Danger:     a.req.Posture == ptyevent.PostureDangerouslyExecuteAll,
		Policy: ptyobs.TracePolicy{
			Rule: res.Decision.Rule, Act: a.req.Act, Autopilot: a.req.IsAutopilot, Downgrade: res.DowngradeReason,
		},
		Action:       string(res.Decision.Action),
		OptionIndex:  res.Decision.OptionIndex,
		Revalidation: res.RevalidationCode,
		Arbitration:  string(res.ArbitrationVerdict),
		Outcome:      outcome,
		Route:        route,
		Reason:       res.Decision.Reason,
		LatencyMS:    latency.Milliseconds(),
		DryRun:       dry,
	}
	if res.DecisionEvent != nil {
		rec.CorrelationID = res.DecisionEvent.ID
		rec.EventSeq = res.DecisionEvent.DecidedAtSeq
		if rec.Reason == "" {
			rec.Reason = res.DecisionEvent.Reason
		}
	}
	if res.Delivered {
		rec.Input = ptyobs.DescribeInput(res.DeliveredBytes, res.Decision.OptionIndex)
	}
	if p != nil {
		rec.Fingerprint, rec.ShapeVersion, rec.InstanceKey = p.Fingerprint, p.ShapeVersion, p.InstanceKey
		rec.Class, rec.Provenance, rec.Confidence, rec.LearnedShapeID = string(p.Class), string(p.Provenance), p.Confidence, p.LearnedShapeID
	}
	switch {
	case err != nil:
		rec.Reason = "router error: delivery or classification failed"
	case a.unreadable != "" && rec.Reason == "":
		rec.Reason = a.unreadable
	case rec.Reason == "" && esc != "":
		rec.Reason = esc
	}
	if t := e.cfg.Trace; t != nil {
		if terr := t.Append(rec); terr == nil {
			e.cfg.Metrics.Inc(ptyobs.MTraceRecords)
		}
	}

	m := e.cfg.Metrics
	if m == nil {
		return
	}
	act := rec.Action
	if act == "" {
		act = "none"
	}
	m.Inc(ptyobs.MDecisions, rec.Adapter, orUnset(rec.Posture), act, string(outcome), string(route))
	m.ObserveDuration(ptyobs.MDecisionLatency, latency, rec.Adapter, string(outcome))
	if esc != "" && (outcome == ptyobs.OutcomeEscalated || outcome == ptyobs.OutcomeAttention || outcome == ptyobs.OutcomeDeferred) {
		m.Inc(ptyobs.MEscalations, rec.Adapter, esc)
	}
	if rec.LearnedShapeID != "" && (outcome == ptyobs.OutcomeApproved || outcome == ptyobs.OutcomeDenied || outcome == ptyobs.OutcomeDryRun) {
		m.Inc(ptyobs.MLearnedShape, rec.Adapter, "used")
	}
	if rec.LearnedShapeID != "" && res.DowngradeReason == "breaker-tripped" {
		m.Inc(ptyobs.MLearnedShape, rec.Adapter, "retired")
	}
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// countPrompt records a recognized prompt (rate denominator for unrecognized).
func (e *Engine) countPrompt(adapter, version string, p *ptyevent.Prompt) {
	m := e.cfg.Metrics
	if m == nil || p == nil {
		return
	}
	cls := string(p.Class)
	m.Inc(ptyobs.MPrompts, adapter, version, cls, string(p.Provenance))
	m.Observe(ptyobs.MPromptConf, p.Confidence, adapter, string(p.Provenance))
}

// countUnrecognized records a blocking screen no parser or learned shape read.
func (e *Engine) countUnrecognized(adapter, version string) {
	m := e.cfg.Metrics
	if m == nil {
		return
	}
	m.Inc(ptyobs.MUnrecognized, adapter, version, "unknown")
	m.Inc(ptyobs.MPrompts, adapter, version, "unknown", "none")
}
