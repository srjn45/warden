package decisionrouter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
)

// Router drives prompt decisions from classification to input delivery and journaling.
type Router struct {
	cfg        Config
	classifier Classifier
	shapeStore LearnedShapeStore
	breaker    *approval.Breaker
	fastbrain  fastbrain.Engine
	consultor  brainconsult.Consultor
	humanInbox HumanInboxNotifier
	clock      func() time.Time
}

// RouterOptions allows configuring optional components on Router.
type RouterOptions struct {
	Config     Config
	Classifier Classifier
	ShapeStore LearnedShapeStore
	Breaker    *approval.Breaker
	FastBrain  fastbrain.Engine
	Consultor  brainconsult.Consultor
	HumanInbox HumanInboxNotifier
	Clock      func() time.Time
}

// New creates a new DecisionRouter.
func New(opts RouterOptions) *Router {
	cfg := opts.Config
	if cfg.MaxDecisionAge <= 0 {
		cfg.MaxDecisionAge = ptyevent.DefaultMaxDecisionAge
	}
	if cfg.Ownership.HumanQuiet <= 0 {
		cfg.Ownership = ptyevent.DefaultOwnership
	}
	if cfg.ShapePolicy.MinObservations <= 0 {
		cfg.ShapePolicy = ptyevent.DefaultShapePolicy
	}
	if cfg.BrainTimeout <= 0 {
		cfg.BrainTimeout = 5 * time.Minute
	}

	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	breaker := opts.Breaker
	if breaker == nil {
		breaker = approval.NewBreaker()
	}

	return &Router{
		cfg:        cfg,
		classifier: opts.Classifier,
		shapeStore: opts.ShapeStore,
		breaker:    breaker,
		fastbrain:  opts.FastBrain,
		consultor:  opts.Consultor,
		humanInbox: opts.HumanInbox,
		clock:      clock,
	}
}

// Route executes the full decision pipeline:
// 1. Classification (if prompt not provided)
// 2. Permission vs Strategic Decision Routing
// 3. Posture Evaluation & Policy Matching (ptyevent.Decide)
// 4. Downgrade Guards (Act Switch, Circuit Breaker, Arbiter)
// 5. TOCTOU Revalidation
// 6. Input Delivery through Arbitration
// 7. DecisionEvent Journaling via Stream.Emit
func (r *Router) Route(ctx context.Context, req RouteRequest) (RouteResult, error) {
	now := r.clock()
	res := RouteResult{}

	// 1. Obtain normalized prompt
	prompt := req.LivePrompt
	if prompt == nil {
		if r.classifier == nil {
			return res, errors.New("no classifier configured and no live prompt provided")
		}
		cres, err := r.classifier.Classify(ctx, ClassifyRequest{
			Screen:            req.Screen,
			Backend:           req.Backend,
			BackendID:         req.BackendID,
			CLIVersion:        req.CLIVersion,
			SessionID:         req.SessionID,
			ProjectID:         req.Workdir,
			Workdir:           req.Workdir,
			TargetInWorkspace: false,
			AgentID:           req.AgentID,
			Now:               now,
		})
		if err != nil {
			return res, fmt.Errorf("prompt classification failed: %w", err)
		}
		if cres.IsInformational {
			return res, nil
		}
		prompt = cres.Prompt
	}
	res.Prompt = prompt

	if prompt == nil {
		return res, nil
	}

	// 2. Evaluate learned shape usability if prompt has learned shape ID
	learnedUsable := false
	if prompt.LearnedShapeID != "" && r.shapeStore != nil {
		if sh, ok := r.shapeStore.Get(prompt.LearnedShapeID); ok {
			learnedUsable = sh.Usable(ptyevent.ShapeQuery{
				Now:        now,
				Backend:    req.BackendID,
				CLIVersion: req.CLIVersion,
				SessionID:  req.SessionID,
				ProjectID:  req.Workdir,
			})
		}
	}

	// 3. Evaluate allowlist match from policy
	allowlistMatch := r.evaluateAllowlist(prompt, req.AllowlistPolicy)

	// 4. Permissions engine: ptyevent.Decide
	dec := ptyevent.Decide(ptyevent.DecideInput{
		Posture:       req.Posture,
		Prompt:        prompt,
		LearnedUsable: learnedUsable,
		Allowlist:     allowlistMatch,
		AllowStanding: req.AllowlistPolicy.AllowSticky,
	})

	// 5. Strategic decisions routing
	if dec.Action == ptyevent.ActionRouteDecision || prompt.Class == ptyevent.ClassDecision {
		if req.IsAutopilot || req.RunID != "" {
			// Autopilot runs route to Tier-1 manager / brain consult
			dec = r.routeToBrainConsult(ctx, req, prompt)
		} else {
			// Plain agents & pipeline jobs route to human approval/inbox UI
			dec = r.routeToHumanInbox(ctx, req, prompt)
		}
	}

	// 6. Act switch check (§10.3)
	// If act is disabled (and not autopilot), surface approve/deny as ask_human
	if !req.Act && !req.IsAutopilot && req.RunID == "" {
		if dec.Action == ptyevent.ActionApprove || dec.Action == ptyevent.ActionDeny {
			origAction := dec.Action
			dec.Action = ptyevent.ActionAskHuman
			dec.Reason = fmt.Sprintf("auto-approve disabled (act=false); suggested: %s", origAction)
			res.Downgraded = true
			res.DowngradeReason = "act-switch-disabled"
		}
	}

	// 7. Downgrade guards: Circuit Breaker and Fast-Brain Arbiter
	// The breaker and arbiter may ONLY downgrade (approve -> ask_human), never upgrade.
	if dec.Action == ptyevent.ActionApprove {
		// Circuit Breaker check
		maxRepeats := req.AllowlistPolicy.EffectiveMaxRepeats()
		ok, _ := r.breaker.Allow(req.SessionID, prompt.Fingerprint, maxRepeats)
		if !ok {
			dec.Action = ptyevent.ActionAskHuman
			dec.Rule = "breaker-tripped"
			dec.Reason = fmt.Sprintf("auto-approve circuit breaker tripped after %d repeats", maxRepeats)
			res.Downgraded = true
			res.DowngradeReason = "breaker-tripped"

			// If prompt used a learned shape, retire it on breaker trip
			if prompt.LearnedShapeID != "" && r.shapeStore != nil {
				_, _ = r.shapeStore.Retire(prompt.LearnedShapeID, now, "breaker tripped on repeated prompt")
			}
		} else if req.AllowlistPolicy.UseFastBrain && r.fastbrain != nil {
			// Arbiter check: may only downgrade approve -> ask_human
			if prompt.HasCap(ptyevent.CapDestructive) && req.Posture != ptyevent.PostureDangerouslyExecuteAll {
				dec.Action = ptyevent.ActionAskHuman
				dec.Rule = ptyevent.RuleDestructiveHuman
				dec.Reason = "destructive prompt blocked by arbiter safety check"
				res.Downgraded = true
				res.DowngradeReason = "arbiter-destructive"
			} else {
				d, err := r.fastbrain.ArbitrateApproval(ctx, fastbrain.ArbiterInput{
					Approval: &agentbackend.Approval{
						Action:      prompt.Subject + "(" + prompt.Target + ")",
						Question:    prompt.Question,
						Options:     extractLabels(prompt.Options),
						SelectedIdx: defaultOptionIndex(prompt.Options),
					},
					AgentID: req.AgentID,
				})
				if err == nil && (d.Action == fastbrain.DecisionEscalate || d.Action == fastbrain.DecisionReject) {
					dec.Action = ptyevent.ActionAskHuman
					dec.Rule = "arbiter-downgraded"
					dec.Reason = "fastbrain arbiter escalated: " + d.Rationale
					res.Downgraded = true
					res.DowngradeReason = "arbiter-escalated"
				}
			}
		}
	}

	res.Decision = dec

	// Determine sequence number from stream journal
	var headSeq uint64
	if req.Stream != nil && req.Stream.Journal() != nil {
		headSeq = req.Stream.Journal().NextSeq()
	}

	// 8. If non-actionable (ask_human, needs_attention, route_decision), journal and exit
	if dec.Action != ptyevent.ActionApprove && dec.Action != ptyevent.ActionDeny {
		decEv := &ptyevent.DecisionEvent{
			ID:           newDecisionID(),
			Fingerprint:  prompt.Fingerprint,
			Posture:      req.Posture,
			Action:       dec.Action,
			OptionIndex:  dec.OptionIndex,
			Rule:         dec.Rule,
			Reason:       dec.Reason,
			DecidedAtSeq: headSeq,
		}
		res.DecisionEvent = decEv
		r.emitDecision(req.Stream, decEv)
		return res, nil
	}

	// 9. TOCTOU Revalidation
	pd := ptyevent.PendingDecision{
		Decision:   dec,
		Prompt:     *prompt,
		DecidedSeq: headSeq,
		DecidedAt:  now,
	}

	liveState := req.LiveState
	if liveState == "" {
		liveState = ptyevent.StateWaitingForPermission
	}

	// Re-read the prompt from the LIVE screen right now. Revalidating the
	// prompt against itself would prove nothing: the point of §11 is that the
	// pane may have changed (a different command in the same box, a redrawn
	// menu, a human who answered first) since the decision was made.
	livePrompt := prompt
	if req.Stream != nil {
		livePrompt = r.readLive(ctx, req, prompt)
	}
	live := ptyevent.Live{
		Prompt:   livePrompt,
		State:    liveState,
		HumanSeq: req.HumanSeq,
		Now:      now,
	}

	// If input writer provides live lease, use its HumanSeq
	if req.InputWriter != nil {
		lease := req.InputWriter.Lease()
		if lease.HumanSeq > live.HumanSeq {
			live.HumanSeq = lease.HumanSeq
		}
	}

	if err := ptyevent.Revalidate(pd, live, r.cfg.MaxDecisionAge); err != nil {
		var re *ptyevent.RevalidationError
		revalCode := "revalidation-failed"
		if errors.As(err, &re) {
			revalCode = re.Code
		}
		res.RevalidationCode = revalCode

		decEv := &ptyevent.DecisionEvent{
			ID:           newDecisionID(),
			Fingerprint:  prompt.Fingerprint,
			Posture:      req.Posture,
			Action:       dec.Action,
			OptionIndex:  dec.OptionIndex,
			Rule:         dec.Rule,
			Reason:       dec.Reason,
			DecidedAtSeq: headSeq,
			Revalidation: revalCode,
		}
		res.DecisionEvent = decEv
		r.emitDecision(req.Stream, decEv)
		return res, nil
	}

	if req.DryRun {
		decEv := &ptyevent.DecisionEvent{
			ID:           newDecisionID(),
			Fingerprint:  prompt.Fingerprint,
			Posture:      req.Posture,
			Action:       dec.Action,
			OptionIndex:  dec.OptionIndex,
			Rule:         dec.Rule,
			Reason:       dec.Reason + " [dry-run: not delivered]",
			DecidedAtSeq: headSeq,
			Revalidation: ptyevent.RevalOK,
		}
		res.DecisionEvent = decEv
		res.RevalidationCode = ptyevent.RevalOK
		r.emitDecision(req.Stream, decEv)
		return res, nil
	}

	// 10. Keystroke generation
	bytesToSend := computeKeystrokes(prompt, dec.OptionIndex)
	res.DeliveredBytes = bytesToSend

	// 11. Input delivery through Arbitration
	decID := newDecisionID()
	var currentLease ptyevent.Lease
	if req.InputWriter != nil {
		currentLease = req.InputWriter.Lease()
	}

	inputReq := ptyevent.InputRequest{
		Origin:     ptyevent.OriginWarden,
		ClientID:   req.SessionID,
		At:         now,
		Seq:        headSeq,
		Data:       bytesToSend,
		DecisionID: decID,
	}

	updatedLease, arb := ptyevent.Arbitrate(currentLease, inputReq, r.cfg.Ownership)
	res.ArbitrationVerdict = arb.Verdict

	if arb.Verdict != ptyevent.VerdictWrite {
		revalCode := string(arb.Verdict)
		res.RevalidationCode = revalCode
		decEv := &ptyevent.DecisionEvent{
			ID:           decID,
			Fingerprint:  prompt.Fingerprint,
			Posture:      req.Posture,
			Action:       dec.Action,
			OptionIndex:  dec.OptionIndex,
			Rule:         dec.Rule,
			Reason:       arb.Reason,
			DecidedAtSeq: headSeq,
			Revalidation: revalCode,
		}
		res.DecisionEvent = decEv
		r.emitDecision(req.Stream, decEv)
		return res, nil
	}

	// Write keystrokes to terminal
	if req.InputWriter != nil {
		if err := req.InputWriter.WriteInput(ctx, bytesToSend, ptyevent.OriginWarden, decID); err != nil {
			return res, fmt.Errorf("failed to write keystrokes: %w", err)
		}
		res.Delivered = true
		_ = ptyevent.Release(updatedLease, ptyevent.OriginWarden)
	}

	// 12. Journal DecisionEvent
	decEv := &ptyevent.DecisionEvent{
		ID:           decID,
		Fingerprint:  prompt.Fingerprint,
		Posture:      req.Posture,
		Action:       dec.Action,
		OptionIndex:  dec.OptionIndex,
		Rule:         dec.Rule,
		Reason:       dec.Reason,
		DecidedAtSeq: headSeq,
		Revalidation: ptyevent.RevalOK,
	}
	res.DecisionEvent = decEv
	res.RevalidationCode = ptyevent.RevalOK
	r.emitDecision(req.Stream, decEv)

	return res, nil
}

func (r *Router) evaluateAllowlist(p *ptyevent.Prompt, pol approval.Policy) ptyevent.AllowlistMatch {
	if !pol.HasRules() {
		return ptyevent.AllowlistNone
	}

	tool := p.Subject
	arg := p.Target
	q := p.Question

	var paths []string
	if arg != "" {
		paths = append(paths, arg)
		for _, tok := range strings.Fields(arg) {
			if strings.ContainsAny(tok, "/.") {
				paths = append(paths, tok)
			}
		}
	}

	// Deny rules win
	for _, rule := range pol.Rules.Deny {
		if ruleMatches(rule, tool, arg, q, paths) {
			return ptyevent.AllowlistDeny
		}
	}

	// Allow rules
	for _, rule := range pol.Rules.Allow {
		if ruleMatches(rule, tool, arg, q, paths) {
			return ptyevent.AllowlistAllow
		}
	}

	return ptyevent.AllowlistNone
}

func ruleMatches(rule approval.Rule, tool, arg, question string, paths []string) bool {
	if rule.Tool != "" && !strings.EqualFold(rule.Tool, tool) {
		return false
	}
	combined := tool + "(" + arg + ")"
	if rule.Pattern != "" {
		if !globMatch(rule.Pattern, combined) && !globMatch(rule.Pattern, question) {
			return false
		}
	}
	if len(rule.Paths) > 0 {
		matched := false
		for _, g := range rule.Paths {
			for _, cand := range paths {
				if globMatch(g, cand) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func globMatch(pat, s string) bool {
	if pat == "*" || pat == "" {
		return true
	}
	p := strings.ToLower(pat)
	target := strings.ToLower(s)
	if strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*") {
		return strings.Contains(target, strings.Trim(p, "*"))
	}
	if strings.HasPrefix(p, "*") {
		return strings.HasSuffix(target, strings.TrimPrefix(p, "*"))
	}
	if strings.HasSuffix(p, "*") {
		return strings.HasPrefix(target, strings.TrimSuffix(p, "*"))
	}
	return p == target
}

func (r *Router) routeToBrainConsult(ctx context.Context, req RouteRequest, p *ptyevent.Prompt) ptyevent.Decision {
	if r.consultor == nil {
		return ptyevent.Decision{
			Action: ptyevent.ActionNeedsAttention,
			Rule:   "brain-consult-unconfigured",
			Reason: "autopilot run prompt requires brain consult but consultor is unconfigured",
		}
	}

	paneTail := req.TranscriptSummary
	if paneTail == "" {
		paneTail = req.Screen
	}
	if len(paneTail) > 4096 {
		paneTail = paneTail[len(paneTail)-4096:]
	}

	evidenceBuilder := strings.Builder{}
	if req.GitState != "" {
		evidenceBuilder.WriteString("Git State:\n" + req.GitState + "\n")
	}
	if len(req.HistoricalDecisions) > 0 {
		evidenceBuilder.WriteString("Historical Decisions:\n" + strings.Join(req.HistoricalDecisions, "\n") + "\n")
	}

	answerCtx := &brainconsult.AnswerContext{
		Question:    p.Question,
		Options:     extractLabels(p.Options),
		PaneTail:    paneTail,
		Task:        req.ActiveTask,
		Constraints: req.Constraints,
		Destructive: p.HasCap(ptyevent.CapDestructive),
	}

	tctx, cancel := context.WithTimeout(ctx, r.cfg.BrainTimeout)
	defer cancel()

	result, err := r.consultor.Consult(tctx, brainconsult.Request{
		Intent:    "answer autopilot strategic decision",
		Situation: req.PlanGoal,
		Goal:      req.PlanGoal,
		Evidence:  evidenceBuilder.String(),
		RunID:     req.RunID,
		TaskID:    req.ActiveTask,
		Repo:      req.Workdir,
		Mode:      brainconsult.ModeAnswer,
		Answer:    answerCtx,
	})

	if err != nil {
		return ptyevent.Decision{
			Action: ptyevent.ActionNeedsAttention,
			Rule:   "brain-consult-failed",
			Reason: fmt.Sprintf("brain consult failed: %v", err),
		}
	}

	switch result.Answer {
	case brainconsult.AnswerSelectOption:
		if result.Option >= 1 && result.Option <= len(p.Options) {
			return ptyevent.Decision{
				Action:      ptyevent.ActionApprove,
				OptionIndex: result.Option,
				Rule:        "brain-consult-approved",
				Reason:      result.Reason,
			}
		}
	case brainconsult.AnswerApprove:
		opt := 1
		for _, o := range p.Options {
			if o.Semantic.Allows() {
				opt = o.Index
				break
			}
		}
		return ptyevent.Decision{
			Action:      ptyevent.ActionApprove,
			OptionIndex: opt,
			Rule:        "brain-consult-approved",
			Reason:      result.Reason,
		}
	case brainconsult.AnswerReject:
		opt := 0
		for _, o := range p.Options {
			if o.Semantic.Refuses() {
				opt = o.Index
				break
			}
		}
		if opt > 0 {
			return ptyevent.Decision{
				Action:      ptyevent.ActionDeny,
				OptionIndex: opt,
				Rule:        "brain-consult-rejected",
				Reason:      result.Reason,
			}
		}
	}

	return ptyevent.Decision{
		Action: ptyevent.ActionNeedsAttention,
		Rule:   "brain-consult-unresolved",
		Reason: "brain consult returned unresolvable decision: " + string(result.Answer),
	}
}

func (r *Router) routeToHumanInbox(ctx context.Context, req RouteRequest, p *ptyevent.Prompt) ptyevent.Decision {
	var safeOptions []string
	for _, o := range p.Options {
		if !o.Semantic.Refuses() && o.Semantic != ptyevent.SemCancel {
			safeOptions = append(safeOptions, o.Label)
		}
	}

	if r.humanInbox != nil {
		_ = r.humanInbox.NotifyHumanApproval(ctx, req.AgentID, req.SessionID, p, safeOptions)
	}

	return ptyevent.Decision{
		Action: ptyevent.ActionAskHuman,
		Rule:   "decision-human-inbox",
		Reason: "strategic decision routed to human inbox",
	}
}

func (r *Router) emitDecision(stream Stream, decEv *ptyevent.DecisionEvent) {
	if stream == nil || decEv == nil {
		return
	}
	ev := ptyevent.Event{
		Kind:     ptyevent.KindDecision,
		Decision: decEv,
	}
	_, _ = stream.Emit(ev)
}

func computeKeystrokes(p *ptyevent.Prompt, targetOptionIndex int) []byte {
	if targetOptionIndex < 1 || targetOptionIndex > len(p.Options) {
		return []byte("\r")
	}

	if !p.Navigate {
		// Hotkey / number menu
		opt := p.Options[targetOptionIndex-1]
		if opt.Key != "" {
			return []byte(opt.Key + "\r")
		}
		return []byte(strconv.Itoa(targetOptionIndex) + "\r")
	}

	// Cursor menu: compute arrow moves relative to current default
	defIdx := 1
	for _, o := range p.Options {
		if o.Default {
			defIdx = o.Index
			break
		}
	}

	delta := targetOptionIndex - defIdx
	var b strings.Builder
	if delta > 0 {
		for i := 0; i < delta; i++ {
			b.WriteString("\x1b[B") // Down
		}
	} else if delta < 0 {
		for i := 0; i < -delta; i++ {
			b.WriteString("\x1b[A") // Up
		}
	}
	b.WriteString("\r") // Confirm
	return []byte(b.String())
}

func extractLabels(options []ptyevent.Option) []string {
	labels := make([]string, len(options))
	for i, o := range options {
		labels[i] = o.Label
	}
	return labels
}

func defaultOptionIndex(options []ptyevent.Option) int {
	for _, o := range options {
		if o.Default {
			return o.Index
		}
	}
	return 1
}

func newDecisionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "dec-" + hex.EncodeToString(b)
}

// readLive re-reads the prompt currently on screen. nil means no prompt is on
// screen (Revalidate reports prompt-gone).
//
// A parser-sourced prompt is re-parsed deterministically from the screen, so a
// reworded question, moved option or different command changes the fingerprint
// or instance key. A learned or Fast-Brain prompt has no deterministic parser
// to re-run (a model call here would be slow and non-reproducible), so its
// own labels are re-located on the live screen and its target must still be
// visible; anything less is a changed prompt.
func (r *Router) readLive(ctx context.Context, req RouteRequest, p *ptyevent.Prompt) *ptyevent.Prompt {
	screen := req.Stream.Screen()
	if strings.TrimSpace(screen) == "" {
		return nil
	}
	if p.Provenance == ptyevent.ProvParser && r.classifier != nil {
		cres, err := r.classifier.Classify(ctx, ClassifyRequest{
			Screen:        screen,
			Backend:       req.Backend,
			BackendID:     req.BackendID,
			CLIVersion:    req.CLIVersion,
			SessionID:     req.SessionID,
			ProjectID:     req.Workdir,
			Workdir:       req.Workdir,
			AgentID:       req.AgentID,
			Now:           r.clock(),
			Deterministic: true,
		})
		if err != nil || cres.IsInformational || cres.Prompt == nil {
			return nil
		}
		return cres.Prompt
	}

	loc, verified := agentbackend.LocateOptions(screen, extractLabels(p.Options))
	if !verified {
		return nil
	}
	cp := *p
	cp.Options = append([]ptyevent.Option(nil), p.Options...)
	for i := range cp.Options {
		cp.Options[i].Default = cp.Options[i].Index == loc.Selected
	}
	cp.Verified = true
	if p.Target != "" && !strings.Contains(screen, p.Target) {
		cp.InstanceKey = ptyevent.InstanceKey(p.Fingerprint, p.Subject, "\x00target-missing")
	}
	return &cp
}
