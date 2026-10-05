package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

// Model-assisted prompt recognition.
//
// A backend's ParseApproval keys on one CLI's exact prompt wording, and vendors
// reword their prompts between releases (Antigravity 1.2 turned "Do you want to
// proceed?" into "Run this command?"). When that happens the prompt is invisible
// to warden: the agent reads as working, nothing reaches the approvals inbox and
// auto-approve never runs. This file is the fallback: a menu-shaped block that
// sits unchanged at the bottom of a pane, and that no parser recognizes, is read
// by the Fast-Brain model and turned into the same neutral Approval a parser
// would have produced.
//
// The model only RECOGNIZES. It never decides and never presses a key: its
// reading is verified against the live pane (every option label must really be
// on screen, in order — agentbackend.LocateOptions) and the resulting Approval
// then goes through the unchanged chain (destructive guard → policy → sticky
// gate → circuit breaker), and is answered by moving the cursor and pressing
// Enter only after a re-capture confirms the cursor is on the chosen option.

// recognizeAfter is how long a menu-shaped block must sit unchanged before the
// model is asked about it. A prompt a parser knows is handled at once; this
// delay only keeps a menu that is being drawn, or output scrolling past, from
// costing a model call. Overridable in tests.
var recognizeAfter = 8 * time.Second

// recognizeMaxAttempts bounds model calls per distinct menu: one retry covers a
// timed-out or unavailable model without re-asking forever about a block the
// model already said is not a prompt.
const recognizeMaxAttempts = 2

// recognition is the poller's memory of one agent's current menu-shaped block.
type recognition struct {
	key      string    // agentbackend.FindMenu key of the block
	since    time.Time // when this block was first seen (or last asked about)
	attempts int
	inflight bool
	ap       *agentbackend.Approval // the verified reading; nil until recognized
}

// SetRecognizePrompts turns model-assisted prompt recognition on or off (config
// recognize_prompts); safe to call while the poller runs. Turning it off forgets
// every model-recognized prompt at once.
func (p *Poller) SetRecognizePrompts(on bool) {
	p.recognizePrompts.Store(on)
	if !on {
		p.recogMu.Lock()
		p.recog = nil
		p.recogMu.Unlock()
	}
}

// ParseApproval is the poller's view of an agent's pending prompt: the backend's
// own parser first, then — when that does not match — a model-recognized prompt
// that is still showing in pane. Every consumer of "what is this agent asking"
// (auto-approve, the trust step, the approvals inbox, manual approve) goes
// through it so a model-recognized prompt behaves exactly like a parsed one.
func (p *Poller) ParseApproval(s *agentstore.Agent, pane string) (*agentbackend.Approval, bool) {
	if ap, ok := p.backendFor(s).ParseApproval(pane); ok && ap != nil {
		return ap, true
	}
	p.recogMu.Lock()
	var known *agentbackend.Approval
	if e := p.recog[s.ID]; e != nil {
		known = e.ap
	}
	p.recogMu.Unlock()
	if known == nil {
		return nil, false
	}
	// Re-verify against THIS pane: the reading is only valid while the same
	// options are on screen, and the cursor position is always read fresh.
	loc, ok := agentbackend.LocateOptions(pane, known.Options)
	if !ok {
		return nil, false
	}
	cp := *known
	cp.SelectedIdx = loc.Selected
	return &cp, true
}

// tryRecognize runs from the tick for every live agent. It is a no-op unless
// recognition is on, a model is wired, no parser recognizes the pane, and a
// menu-shaped block has been sitting unchanged for recognizeAfter; then it asks
// the model once (in the background — a model call must never stall the tick).
func (p *Poller) tryRecognize(ctx context.Context, s *agentstore.Agent, pane string) {
	if !p.recognizePrompts.Load() || p.FastBrain == nil {
		return
	}
	var key string
	if ap, ok := p.backendFor(s).ParseApproval(pane); !ok || ap == nil {
		key, _ = agentbackend.FindMenu(pane)
	}
	now := time.Now()

	p.recogMu.Lock()
	if key == "" {
		delete(p.recog, s.ID) // a parser has it, or there is no menu any more
		p.recogMu.Unlock()
		return
	}
	e := p.recog[s.ID]
	if e == nil || e.key != key {
		if p.recog == nil {
			p.recog = map[string]*recognition{}
		}
		e = &recognition{key: key, since: now}
		p.recog[s.ID] = e
	}
	if e.ap != nil || e.inflight || e.attempts >= recognizeMaxAttempts || now.Sub(e.since) < recognizeAfter {
		p.recogMu.Unlock()
		return
	}
	e.inflight = true
	e.attempts++
	p.recogMu.Unlock()

	p.recogWG.Add(1)
	go func() {
		defer p.recogWG.Done()
		p.recognize(ctx, s, pane, key)
	}()
}

// recognize makes the model call for one menu and stores a verified reading.
func (p *Poller) recognize(ctx context.Context, s *agentstore.Agent, pane, key string) {
	var ap *agentbackend.Approval
	r, ok, err := fastbrain.RecognizePrompt(ctx, p.FastBrain, pane)
	if err != nil {
		slog.Warn("prompt recognition error", "agent", s.ID, "err", err)
	}
	if ok {
		if ap = recognizedApproval(pane, r); ap == nil {
			slog.Info("prompt recognition discarded: the model's options are not on screen", "agent", s.ID)
		}
	}

	p.recogMu.Lock()
	e := p.recog[s.ID]
	if e == nil || e.key != key {
		p.recogMu.Unlock()
		return // the pane moved on while the model was thinking
	}
	e.inflight = false
	e.since = time.Now() // a retry waits out recognizeAfter again
	e.ap = ap
	p.recogMu.Unlock()
	if ap == nil {
		return
	}

	slog.Info("prompt recognized by model", "agent", s.ID, "question", ap.Question,
		"options", len(ap.Options), "confidence", r.Confidence)
	_ = p.deps.RecordEvent(ctx, s.ID, store.Event{
		TS:   time.Now(),
		Type: "prompt_recognized",
		Detail: fmt.Sprintf("no parser matched this prompt; a model read it as %q with %d options (confidence %.2f)",
			ap.Question, len(ap.Options), r.Confidence),
	})
	// Hand it to the same worker a parsed prompt reaches, and let the next tick
	// flip the status to waiting_for_input.
	p.publishApprovalEvent(s, pane)
	if p.OnChange != nil {
		p.OnChange()
	}
}

// recognizedBlockLines is how far above the options the destructive guard reads
// for a model-recognized prompt.
const recognizedBlockLines = 12

// recognizedApproval turns a model's reading into an Approval, keeping only what
// the pane itself confirms. It returns nil when the options are not on screen.
//
// Nothing the model says can lower the bar the deterministic chain applies:
//   - options must locate in the pane verbatim and in order;
//   - question and action are kept only if they appear in the pane;
//   - if ANY text in the block above the options trips the destructive guard, the
//     whole block becomes the Action, so the guard fires even if the model
//     summarized the command away;
//   - an "affirmative" whose label reads as a refusal is dropped, and a label
//     that reads as a standing grant is sticky whatever the model claimed;
//   - "trust" (which is answered without the auto-approve policy) is accepted
//     only when the block itself talks about trust.
func recognizedApproval(pane string, r fastbrain.Recognition) *agentbackend.Approval {
	loc, ok := agentbackend.LocateOptions(pane, r.Options)
	if !ok {
		return nil
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	from := loc.First - recognizedBlockLines
	if from < 0 {
		from = 0
	}
	var above []string
	for _, l := range lines[from:loc.First] {
		if t := strings.TrimSpace(l); t != "" {
			above = append(above, t)
		}
	}
	block := strings.Join(above, "\n")

	ap := &agentbackend.Approval{
		Options:           r.Options,
		SelectedIdx:       loc.Selected,
		AffirmativeIdx:    r.Affirmative,
		AffirmativeSticky: r.Sticky,
		// Enter on a verified cursor works for hotkey and cursor menus alike;
		// whether a digit confirms or merely moves the cursor is the CLI's secret.
		Navigate: true,
		Inferred: true,
	}
	if r.Question != "" && strings.Contains(pane, r.Question) {
		ap.Question = r.Question
	} else if len(above) > 0 {
		ap.Question = above[len(above)-1]
	}
	if r.Action != "" && strings.Contains(pane, r.Action) {
		ap.Action = r.Action
	}
	if bad, _ := approval.IsDestructive(approval.Approval{Action: block}); bad {
		ap.Action = block
	}

	if i := ap.AffirmativeIdx; i > 0 {
		label := strings.ToLower(ap.Options[i-1])
		switch {
		case refusalLabel(label):
			ap.AffirmativeIdx, ap.AffirmativeSticky = 0, false
		case standingLabel(label):
			ap.AffirmativeSticky = true
		}
	}
	if r.Trust && strings.Contains(strings.ToLower(block+"\n"+ap.Question), "trust") {
		ap.Kind = agentbackend.ApprovalKindTrust
		ap.AffirmativeSticky = ap.AffirmativeIdx > 0
	}
	return ap
}

func refusalLabel(low string) bool {
	for _, w := range []string{"no", "cancel", "deny", "reject", "refuse", "decline", "never", "quit", "exit", "abort", "skip", "don't", "do not", "stop"} {
		if low == w || strings.HasPrefix(low, w+" ") || strings.HasPrefix(low, w+",") {
			return true
		}
	}
	return false
}

func standingLabel(low string) bool {
	for _, w := range []string{"always", "don't ask again", "do not ask again", "persist", "remember", "trust"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// recognizedLive reports whether a model-recognized prompt is showing in pane
// (and no parser claims it). The tick uses it to report waiting_for_input for a
// prompt the backend's own state detection does not know.
func (p *Poller) recognizedLive(s *agentstore.Agent, pane string) bool {
	ap, ok := p.ParseApproval(s, pane)
	return ok && ap != nil && ap.Inferred
}

// pruneRecognitions forgets agents that are gone.
func (p *Poller) pruneRecognitions(sessions []*agentstore.Agent) {
	p.recogMu.Lock()
	defer p.recogMu.Unlock()
	if len(p.recog) == 0 {
		return
	}
	live := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		live[s.ID] = true
	}
	for id := range p.recog {
		if !live[id] {
			delete(p.recog, id)
		}
	}
}
