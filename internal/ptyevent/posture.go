package ptyevent

// Posture is a session's explicit permission posture. It replaces the implicit
// "auto-approve on/off + rules + trust_workspace" combination with one named
// level per session (the existing policy maps onto it; see the spec's §9.4).
type Posture string

const (
	// PostureReadOnly approves read-only permissions; it DENIES anything that
	// would write, execute, reach the network, publish or destroy (the posture
	// forbids it), and leaves workspace trust to a human.
	PostureReadOnly Posture = "read-only"
	// PostureReadWrite approves reads, writes and trusting the session's own
	// worktree; everything else goes to a human.
	PostureReadWrite Posture = "read-write"
	// PostureAllowlisted approves reads, own-worktree trust, and whatever the
	// configured allowlist matches — except destructive actions, which always go
	// to a human; everything else goes to a human.
	PostureAllowlisted Posture = "allowlisted"
	// PostureDangerouslyExecuteAll approves every CONFIDENTLY classified
	// permission, including trust and destructive ones. It never answers a
	// decision prompt and never acts on an unconfident classification.
	PostureDangerouslyExecuteAll Posture = "dangerously-execute-all"
)

// Valid reports whether p is known.
func (p Posture) Valid() bool {
	switch p {
	case PostureReadOnly, PostureReadWrite, PostureAllowlisted, PostureDangerouslyExecuteAll:
		return true
	}
	return false
}

// Action is what the router does with a prompt.
type Action string

const (
	// ActionApprove: write the chosen allow option.
	ActionApprove Action = "approve"
	// ActionDeny: write the chosen refusal option.
	ActionDeny Action = "deny"
	// ActionAskHuman: a well-understood permission the posture does not
	// pre-approve; surface it in the approvals inbox (state stays
	// waiting_for_permission).
	ActionAskHuman Action = "ask_human"
	// ActionRouteDecision: a decision prompt; hand to the decision router
	// (brain consult → human). State is waiting_for_decision.
	ActionRouteDecision Action = "route_decision"
	// ActionNeedsAttention: the classification is not trustworthy; never act,
	// surface prominently (state needs_attention).
	ActionNeedsAttention Action = "needs_attention"
)

// AllowlistMatch is the result of evaluating the configured rules (the
// existing approval.Policy allow/deny rules) against the prompt.
type AllowlistMatch string

const (
	AllowlistNone  AllowlistMatch = ""
	AllowlistAllow AllowlistMatch = "allow"
	AllowlistDeny  AllowlistMatch = "deny"
)

// DecideInput is everything Decide reads. It is a value: the router captures
// it once, decides, and later revalidates against the live prompt.
type DecideInput struct {
	Posture Posture
	Prompt  *Prompt
	// LearnedUsable is the store's verdict on Prompt.LearnedShapeID (Usable).
	LearnedUsable bool
	Allowlist     AllowlistMatch
	// AllowStanding permits picking a standing grant (allow_session /
	// allow_always) when no one-time allow exists (today: allow_sticky).
	AllowStanding bool
}

// Decision is Decide's output.
type Decision struct {
	Action      Action `json:"action"`
	OptionIndex int    `json:"option_index,omitempty"` // for approve/deny
	Rule        string `json:"rule"`                   // stable rule id (spec §9 table)
	Reason      string `json:"reason"`
}

// Stable decision rule ids.
const (
	RulePostureInvalid    = "posture-invalid"
	RuleUnconfident       = "unconfident"
	RuleDecisionPrompt    = "decision-never-auto"
	RuleAllowlistDeny     = "allowlist-deny"
	RuleDenyNoPath        = "deny-no-refusal-option"
	RulePermitted         = "posture-permits"
	RuleNoAllowOption     = "no-least-privilege-allow"
	RuleForbiddenDeny     = "posture-forbids"
	RuleForbiddenNoPath   = "posture-forbids-no-refusal-option"
	RuleNotPreapproved    = "posture-not-preapproved"
	RuleTrustNeedsHuman   = "trust-needs-human"
	RuleDestructiveHuman  = "destructive-needs-human"
	RuleUnmappedCapHuman  = "unmapped-capability"
	RuleClassUnknown      = "class-unknown"
	RuleNilPrompt         = "no-prompt"
	RuleStandingForbidden = "standing-grant-not-allowed"
)

// Decide maps posture × classification → action. It is total and pure; the
// full matrix is pinned by testdata/decisions.golden.json. In order:
//
//  1. invalid posture, no prompt, unknown class, or unconfident → needs_attention
//  2. decision prompts → route_decision (every posture, including
//     dangerously-execute-all)
//  3. dangerously-execute-all approves every classified permission using an
//     extracted allow option. Legacy allow/deny rules never constrain it.
//  4. an allowlist DENY match → deny, or ask_human when the prompt offers no
//     refusal option
//  5. every requested capability permitted by the posture → approve with the
//     least-privilege allow option (once ≻ session ≻ always; standing only when
//     allowed — see standingOK), else ask_human
//  5. otherwise: read-only denies (ask_human for trust / when it cannot refuse);
//     the other postures ask a human.
func Decide(in DecideInput) Decision {
	p := in.Prompt
	switch {
	case !in.Posture.Valid():
		return Decision{Action: ActionNeedsAttention, Rule: RulePostureInvalid, Reason: "unknown posture " + string(in.Posture)}
	case p == nil:
		return Decision{Action: ActionNeedsAttention, Rule: RuleNilPrompt, Reason: "no classified prompt"}
	case p.Class == ClassUnknown:
		return Decision{Action: ActionNeedsAttention, Rule: RuleClassUnknown, Reason: "classifier could not tell permission from decision"}
	case !Confident(p, in.LearnedUsable):
		return Decision{Action: ActionNeedsAttention, Rule: RuleUnconfident, Reason: "classification below confidence floor or unverified"}
	case p.Class == ClassDecision:
		return Decision{Action: ActionRouteDecision, Rule: RuleDecisionPrompt, Reason: "strategic/multiple-choice prompts are never auto-answered"}
	}
	// This posture is intentionally loud because it is exceptional: it is the
	// one explicit operator choice that is not filtered through the legacy rule
	// policy. It still needs a confidently extracted allow option; Warden never
	// invents a menu index or answers a strategic/unclassified prompt.
	if in.Posture == PostureDangerouslyExecuteAll {
		idx, rule := allowOption(p, true)
		if idx == 0 {
			return Decision{Action: ActionAskHuman, Rule: rule, Reason: "dangerously-execute-all found no extracted allow option"}
		}
		return Decision{Action: ActionApprove, OptionIndex: idx, Rule: RulePermitted, Reason: "dangerously-execute-all approves every classified permission"}
	}

	if in.Allowlist == AllowlistDeny {
		if idx := refusalOption(p); idx > 0 {
			return Decision{Action: ActionDeny, OptionIndex: idx, Rule: RuleAllowlistDeny, Reason: "matched a deny rule"}
		}
		return Decision{Action: ActionAskHuman, Rule: RuleDenyNoPath, Reason: "matched a deny rule but the prompt offers no refusal option"}
	}

	blocked, why := firstForbidden(in.Posture, p, in.Allowlist == AllowlistAllow)
	if blocked == "" {
		idx, rule := allowOption(p, standingOK(in))
		if idx == 0 {
			return Decision{Action: ActionAskHuman, Rule: rule, Reason: "no allow option fits the least-privilege rule"}
		}
		return Decision{Action: ActionApprove, OptionIndex: idx, Rule: RulePermitted, Reason: "posture permits every requested capability"}
	}

	if in.Posture == PostureReadOnly && blocked != CapTrust && blocked != CapOther {
		if idx := refusalOption(p); idx > 0 {
			return Decision{Action: ActionDeny, OptionIndex: idx, Rule: RuleForbiddenDeny, Reason: "read-only posture forbids " + string(blocked)}
		}
		return Decision{Action: ActionAskHuman, Rule: RuleForbiddenNoPath, Reason: "read-only posture forbids " + string(blocked) + " but the prompt offers no refusal option"}
	}
	return Decision{Action: ActionAskHuman, Rule: why, Reason: string(in.Posture) + " does not pre-approve " + string(blocked)}
}

// firstForbidden returns the first requested capability the posture does not
// pre-approve ("" when all are permitted) and the rule id explaining it.
func firstForbidden(posture Posture, p *Prompt, allowlisted bool) (Capability, string) {
	for _, c := range p.Capabilities {
		if ok, rule := permits(posture, c, p, allowlisted); !ok {
			return c, rule
		}
	}
	return "", ""
}

// permits is the posture × capability table (spec §9.2).
func permits(posture Posture, c Capability, p *Prompt, allowlisted bool) (bool, string) {
	if posture == PostureDangerouslyExecuteAll {
		return true, ""
	}
	switch c {
	case CapRead:
		return true, ""
	case CapTrust:
		if posture == PostureReadOnly || !p.TargetInWorkspace {
			return false, RuleTrustNeedsHuman
		}
		return true, ""
	case CapOther:
		return false, RuleUnmappedCapHuman
	case CapDestructive:
		return false, RuleDestructiveHuman
	case CapWrite:
		if posture == PostureReadWrite || (posture == PostureAllowlisted && allowlisted) {
			return true, ""
		}
	default: // execute, network, credential, publish
		if posture == PostureAllowlisted && allowlisted {
			return true, ""
		}
	}
	return false, RuleNotPreapproved
}

// standingOK reports whether a standing grant may be chosen. Trust prompts are
// inherently standing ("Yes, I trust this folder"), so a permitted trust-only
// prompt may take its allow_always; dangerously-execute-all may always.
func standingOK(in DecideInput) bool {
	if in.AllowStanding || in.Posture == PostureDangerouslyExecuteAll {
		return true
	}
	p := in.Prompt
	return len(p.Capabilities) == 1 && p.Capabilities[0] == CapTrust
}

// allowOption picks the least-privilege allow option: allow_once, then
// allow_session, then allow_always; standing ones only when standing is OK.
func allowOption(p *Prompt, standing bool) (int, string) {
	for _, want := range []OptionSemantic{SemAllowOnce, SemAllowSession, SemAllowAlways} {
		for _, o := range p.Options {
			if o.Semantic == want {
				if want != SemAllowOnce && !standing {
					return 0, RuleStandingForbidden
				}
				return o.Index, ""
			}
		}
	}
	return 0, RuleNoAllowOption
}

// refusalOption picks a plain deny, then deny-with-feedback; never cancel
// (cancel aborts the turn or exits the CLI — a human's call).
func refusalOption(p *Prompt) int {
	for _, want := range []OptionSemantic{SemDeny, SemDenyFeedback} {
		for _, o := range p.Options {
			if o.Semantic == want {
				return o.Index
			}
		}
	}
	return 0
}
