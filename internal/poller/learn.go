package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/store"
)

// Teaching the known-prompts store, and keeping it honest.
//
// Learning policy: a model-recognized prompt (Approval.Inferred, read by the
// Fast-Brain and not by a parser or the store) is learned on its FIRST verified
// success — no multi-sighting threshold. "Verified success" means the prompt was
// really answered (agentbackend.Answer returned nil: by auto-approve, the
// arbiter, the brain chain, the trust step, or a human via the approve endpoint)
// and a LATER capture, at least learnVerifyAfter on, no longer shows that menu.
// A reading that failed verification never becomes an Approval; an answer that
// returned ErrPromptChanged is never reported here; a menu that is still showing
// teaches nothing. One sighting is enough because a stored shape only says how to
// READ a prompt: every use is re-verified against the live pane
// (agentbackend.LocateMatching + readingApproval) and the approval chain is
// unchanged, so a wrong entry cannot answer anything the model's own reading
// could not.
//
// Self-healing: a store-recognized prompt that trips the approve circuit
// breaker is invalidated at once, and one whose answers keep failing to clear
// the menu is invalidated after knownMaxStrikes consecutive failures; the next
// occurrence goes back to the Fast-Brain. Invalidation is a delete, and records
// a prompt_known_invalidated event.

// learnVerifyAfter is how long after an answer the menu must be gone before it
// counts as cleared: the CLI needs time to redraw. Overridable in tests.
var learnVerifyAfter = 2 * time.Second

// learnExpire abandons an unverified answer (the agent is not being polled, or
// the pane never settled) rather than judging it long after the fact.
const learnExpire = time.Minute

// knownMaxStrikes is how many consecutive answers of a store-recognized prompt
// may leave the menu showing before its entry is invalidated.
const knownMaxStrikes = 3

// knownPruneEvery throttles the store's age/size pruning from the tick.
const knownPruneEvery = time.Hour

// pendingLearn is one answered, model- or store-read prompt awaiting proof that
// it cleared.
type pendingLearn struct {
	at      time.Time
	ap      agentbackend.Approval
	backend string
	storeID string // set when the store (not the model) read this prompt
}

// NoteAnswered tells the poller that ap — the pending prompt of agent s, as read
// from pane — was just answered successfully. Callers invoke it only after
// agentbackend.Answer returned nil. Prompts a backend parser read are ignored.
func (p *Poller) NoteAnswered(s *agentstore.Agent, ap *agentbackend.Approval, pane string) {
	if p.Known == nil || ap == nil || !ap.Inferred || !p.recognizePrompts.Load() {
		return
	}
	pl := &pendingLearn{at: time.Now(), ap: *ap, backend: p.backendFor(s).ID()}
	pl.storeID = p.knownID(s, pane)
	p.learnMu.Lock()
	if p.pendingLearns == nil {
		p.pendingLearns = map[string]*pendingLearn{}
	}
	p.pendingLearns[s.ID] = pl
	p.learnMu.Unlock()
}

// knownID is the ID of the store entry that reads pane, or "".
func (p *Poller) knownID(s *agentstore.Agent, pane string) string {
	if p.Known == nil || pane == "" {
		return ""
	}
	if m, ok := p.Known.Match(p.backendFor(s).ID(), pane); ok {
		return m.Entry.ID
	}
	return ""
}

// checkLearn runs from the tick with a fresh capture. Once the verify delay has
// passed it judges the agent's pending answer: menu gone → learn (or confirm the
// known entry); menu still showing → learn nothing (or strike the known entry).
func (p *Poller) checkLearn(ctx context.Context, s *agentstore.Agent, pane string) {
	p.learnMu.Lock()
	pl := p.pendingLearns[s.ID]
	if pl == nil {
		p.learnMu.Unlock()
		return
	}
	age := time.Since(pl.at)
	if age < learnVerifyAfter {
		p.learnMu.Unlock()
		return
	}
	delete(p.pendingLearns, s.ID)
	p.learnMu.Unlock()
	if age > learnExpire || p.Known == nil {
		return
	}

	if _, showing := agentbackend.LocateOptions(pane, pl.ap.Options); showing {
		if pl.storeID != "" {
			p.strikeKnown(ctx, s, pl.storeID, "its answer did not clear the menu")
		}
		return
	}
	if pl.storeID != "" {
		p.learnMu.Lock()
		delete(p.knownStrikes, pl.storeID)
		p.learnMu.Unlock()
		return
	}
	p.recogWG.Add(1)
	go func() {
		defer p.recogWG.Done()
		p.learn(context.WithoutCancel(ctx), s, pl)
	}()
}

// learn persists the template of a prompt whose answer was verified to work.
func (p *Poller) learn(ctx context.Context, s *agentstore.Agent, pl *pendingLearn) {
	ap := pl.ap
	sticky := make([]bool, len(ap.Options))
	if i := ap.AffirmativeIdx; i >= 1 && i <= len(sticky) {
		sticky[i-1] = ap.AffirmativeSticky
	}
	r := knownprompts.Reading{
		Question:    ap.Question,
		Action:      ap.Action,
		Options:     ap.Options,
		Affirmative: ap.AffirmativeIdx,
		Sticky:      sticky,
	}
	if ap.Kind == agentbackend.ApprovalKindTrust {
		r.Kind = knownprompts.KindTrust
	}
	e, created, err := p.Known.Learn(ctx, pl.backend, p.Version, r)
	if err != nil {
		slog.Warn("known prompt not learned", "agent", s.ID, "err", err)
		return
	}
	if !created {
		return
	}
	slog.Info("prompt learned", "agent", s.ID, "id", e.ID, "options", len(e.Options))
	_ = p.deps.RecordEvent(ctx, s.ID, store.Event{
		TS:   time.Now(),
		Type: "prompt_learned",
		Detail: fmt.Sprintf("the model-read prompt was answered and cleared; its shape (%s, %d options) is now known and needs no model call next time",
			e.ID, len(e.Options)),
	})
}

// strikeKnown counts one failed answer of a store-recognized prompt and
// invalidates the entry at knownMaxStrikes consecutive failures.
func (p *Poller) strikeKnown(ctx context.Context, s *agentstore.Agent, id, why string) {
	p.learnMu.Lock()
	if p.knownStrikes == nil {
		p.knownStrikes = map[string]int{}
	}
	p.knownStrikes[id]++
	n := p.knownStrikes[id]
	p.learnMu.Unlock()
	if n >= knownMaxStrikes {
		p.invalidateKnown(ctx, s, id, fmt.Sprintf("%s %d times in a row", why, n))
	}
}

// invalidateKnownFor invalidates the store entry that read pane, if any. The
// circuit-breaker paths call it when a store-recognized prompt trips the breaker.
func (p *Poller) invalidateKnownFor(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, pane, why string) {
	if ap == nil || !ap.Inferred {
		return
	}
	if id := p.knownID(s, pane); id != "" {
		p.invalidateKnown(ctx, s, id, why)
	}
}

// invalidateKnown forgets a store entry so the next occurrence of the prompt is
// read by the Fast-Brain again, and records an event.
func (p *Poller) invalidateKnown(ctx context.Context, s *agentstore.Agent, id, why string) {
	p.learnMu.Lock()
	delete(p.knownStrikes, id)
	p.learnMu.Unlock()
	if err := p.Known.Delete(context.WithoutCancel(ctx), id); err != nil {
		slog.Warn("known prompt not invalidated", "agent", s.ID, "id", id, "err", err)
		return
	}
	// The next menu of this shape must be allowed a fresh model reading.
	p.recogMu.Lock()
	if e := p.recog[s.ID]; e != nil {
		e.knownKey, e.attempts, e.since = "", 0, time.Now()
	}
	p.recogMu.Unlock()
	slog.Warn("known prompt invalidated", "agent", s.ID, "id", id, "why", why)
	_ = p.deps.RecordEvent(ctx, s.ID, store.Event{
		TS:     time.Now(),
		Type:   "prompt_known_invalidated",
		Detail: fmt.Sprintf("known prompt shape %s dropped (%s); the next occurrence goes back to the model", id, why),
	})
}

// pruneLearning forgets state of agents that are gone and, at most hourly, ages
// out and size-bounds the store.
func (p *Poller) pruneLearning(live map[string]bool) {
	p.learnMu.Lock()
	for id := range p.pendingLearns {
		if !live[id] {
			delete(p.pendingLearns, id)
		}
	}
	due := p.Known != nil && time.Since(p.lastKnownPrune) >= knownPruneEvery
	if due {
		p.lastKnownPrune = time.Now()
	}
	p.learnMu.Unlock()
	if due {
		if n := p.Known.Prune(); n > 0 {
			slog.Info("known prompts pruned", "removed", n)
		}
	}
}
