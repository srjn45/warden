package fastbrain

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
)

// ArbiterConfidenceThreshold is the minimum model confidence to act (spec).
const ArbiterConfidenceThreshold = 0.8

// ArbiterAction is what the arbiter decided to do with a pending prompt.
type ArbiterAction string

const (
	DecisionApprove      ArbiterAction = "approve"
	DecisionReject       ArbiterAction = "reject"
	DecisionSelectOption ArbiterAction = "select_option"
	DecisionEscalate     ArbiterAction = "escalate"
)

// ArbiterInput is a pending prompt plus optional context for the model.
type ArbiterInput struct {
	Approval    *agentbackend.Approval
	Goal        string
	Constraints string
	AgentID     string
}

// ArbiterDecision is the arbiter's verdict. Escalate means "leave it to a human".
type ArbiterDecision struct {
	Action         ArbiterAction
	SelectedOption int // 1-based; set only for DecisionSelectOption
	Confidence     float64
	Rationale      string
	Category       PromptCategory
	Tier           Tier // zero when no model was consulted
}

func escalate(cat PromptCategory, tier Tier, resp Response, why string) ArbiterDecision {
	r := resp.Rationale
	if why != "" {
		r = why
	}
	return ArbiterDecision{Action: DecisionEscalate, Confidence: resp.Confidence, Rationale: r, Category: cat, Tier: tier}
}

// ArbitrateApproval is a thin wrapper over the Engine method: it decides a pending prompt via Engine.Decide. Destructive
// prompts escalate before any model call; every failure mode fails open to
// DecisionEscalate. An error is returned only for invalid input.
func ArbitrateApproval(ctx context.Context, e Engine, in ArbiterInput) (ArbiterDecision, error) {
	if e == nil {
		return ArbiterDecision{}, fmt.Errorf("%w: nil engine", ErrInvalidRequest)
	}
	return arbitrate(ctx, e, in)
}

// ArbitrateApproval implements Engine.
func (e *engine) ArbitrateApproval(ctx context.Context, in ArbiterInput) (ArbiterDecision, error) {
	return arbitrate(ctx, e, in)
}

// decider is the single evaluation path the arbiter is built on.
type decider interface {
	Decide(ctx context.Context, req Request) (Response, error)
}

func arbitrate(ctx context.Context, e decider, in ArbiterInput) (ArbiterDecision, error) {
	if e == nil {
		return ArbiterDecision{}, fmt.Errorf("%w: nil engine", ErrInvalidRequest)
	}
	if in.Approval == nil {
		return ArbiterDecision{}, fmt.Errorf("%w: nil approval", ErrInvalidRequest)
	}
	a := in.Approval
	cat := ClassifyPrompt(a)

	// SAFETY: non-overridable, before any AI call.
	if destructive, marker := approval.IsDestructive(approval.Approval{
		Action: a.Action, Question: a.Question, Options: a.Options,
		SelectedIdx: a.SelectedIdx, AffirmativeIdx: a.AffirmativeIdx, AffirmativeSticky: a.AffirmativeSticky,
	}); destructive {
		return ArbiterDecision{Action: DecisionEscalate, Category: cat, Rationale: "destructive action: " + marker}, nil
	}

	req := Request{
		Kind:     KindArbitrateApproval,
		Input:    in,
		Metadata: map[string]string{"agent_id": in.AgentID},
	}
	if cat == CategoryToolPermission {
		req.Tier, req.Prompt = TierFast, toolPermissionPrompt(in)
	} else {
		req.Tier, req.Prompt = TierThinking, strategicQuestionPrompt(in)
	}

	resp, err := e.Decide(ctx, req)
	if err != nil {
		return ArbiterDecision{}, err
	}
	if !resp.OK() {
		return escalate(cat, req.Tier, resp, "fail-open: "+string(resp.Status)), nil
	}

	if cat == CategoryToolPermission {
		var out struct {
			Approve *bool `json:"approve"`
		}
		if json.Unmarshal(resp.Output.Parsed, &out) != nil || out.Approve == nil || resp.Confidence < ArbiterConfidenceThreshold {
			return escalate(cat, req.Tier, resp, ""), nil
		}
		d := ArbiterDecision{Action: DecisionReject, Confidence: resp.Confidence, Rationale: reason(resp), Category: cat, Tier: req.Tier}
		if *out.Approve {
			d.Action = DecisionApprove
		}
		return d, nil
	}

	var out struct {
		Selected *int `json:"selected_option"`
	}
	if json.Unmarshal(resp.Output.Parsed, &out) != nil || out.Selected == nil ||
		*out.Selected < 1 || *out.Selected > len(a.Options) || resp.Confidence < ArbiterConfidenceThreshold {
		return escalate(cat, req.Tier, resp, ""), nil
	}
	return ArbiterDecision{Action: DecisionSelectOption, SelectedOption: *out.Selected,
		Confidence: resp.Confidence, Rationale: reason(resp), Category: cat, Tier: req.Tier}, nil
}

// reason returns the model's rationale, accepting "reason" as an alias.
func reason(resp Response) string {
	if resp.Rationale != "" {
		return resp.Rationale
	}
	var m struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(resp.Output.Parsed, &m)
	return m.Reason
}
