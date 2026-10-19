package stateclassifier

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyobs"
	"github.com/srjn45/warden/internal/store"
)

// Classifier is the event-driven activity/quiescence state classifier.
type Classifier struct {
	cfg         Config
	mgr         Manager
	store       Store
	backends    BackendResolver
	procs       ProcessInspector
	transcripts TranscriptInspector

	mu       sync.Mutex
	sessions map[string]*sessionState
	closed   bool

	// OnTransition is called whenever a transition commits.
	OnTransition func(name string, ch *ptyevent.StateChange)
	// OnPrompt is called on every investigation that finds a normalized prompt
	// on screen (not only the first), so a decision that was deferred can be
	// retried. It runs under the session lock and MUST NOT block.
	OnPrompt func(name string, p *ptyevent.Prompt)
	// OnUnreadable is called when the backend reports a blocking screen that no
	// parser (and no usable learned shape) could read. Same contract as OnPrompt.
	OnUnreadable func(name, screen string)
	// Metrics, when set, counts transitions, investigations and detection
	// latency (bounded labels; see internal/ptyobs).
	Metrics *ptyobs.Metrics
	// OnDisagreement is called in shadow mode when classifier state differs from store status.
	OnDisagreement func(name string, classifierState ptyevent.State, pollerStatus store.Status)
}

type sessionState struct {
	name          string
	rootPID       int
	workdir       string
	snapshot      ptyevent.Snapshot
	lastActivity  time.Time
	lastInput     time.Time
	quiescence    *time.Timer
	currentPrompt *ptyevent.Prompt
	trail         []TransitionRecord
	mu            sync.Mutex
}

// New constructs a Classifier.
func New(cfg Config, mgr Manager, st Store, br BackendResolver, pi ProcessInspector, ti TranscriptInspector) *Classifier {
	if cfg.QuiescenceGrace <= 0 {
		cfg.QuiescenceGrace = 1500 * time.Millisecond
	}
	if cfg.InvestigationTimeout <= 0 {
		cfg.InvestigationTimeout = 2 * time.Second
	}
	if pi == nil {
		pi = NewOSProcessInspector()
	}
	c := &Classifier{
		cfg:         cfg,
		mgr:         mgr,
		store:       st,
		backends:    br,
		procs:       pi,
		transcripts: ti,
		sessions:    make(map[string]*sessionState),
	}
	if mgr != nil {
		mgr.ObserveAgents(c.OnPTYEvent)
	}
	return c
}

// CurrentState returns the current state machine snapshot for name.
func (c *Classifier) CurrentState(name string) (ptyevent.Snapshot, bool) {
	c.mu.Lock()
	s, ok := c.sessions[name]
	c.mu.Unlock()
	if !ok {
		return ptyevent.Snapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, true
}

// History returns the explainable transition trail for name.
func (c *Classifier) History(name string) []TransitionRecord {
	c.mu.Lock()
	s, ok := c.sessions[name]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TransitionRecord, len(s.trail))
	copy(out, s.trail)
	return out
}

func (c *Classifier) getOrCreate(name string) *sessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[name]; ok {
		return s
	}
	now := time.Now()
	s := &sessionState{
		name:         name,
		snapshot:     ptyevent.NewSnapshot(now),
		lastActivity: now,
	}
	s.quiescence = time.AfterFunc(c.cfg.QuiescenceGrace, func() {
		c.onQuiescence(name)
	})
	c.sessions[name] = s
	return s
}

// OnPTYEvent handles one journaled event from ptyhost.Manager.
func (c *Classifier) OnPTYEvent(name string, ev ptyevent.Event) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	s := c.getOrCreate(name)
	s.mu.Lock()
	defer s.mu.Unlock()

	at := ev.Wall
	if at.IsZero() {
		at = time.Now()
	}

	var evs []ptyevent.Evidence

	switch ev.Kind {
	case ptyevent.KindOutput:
		// Active output resets quiescence timer
		s.lastActivity = at
		s.quiescence.Reset(c.cfg.QuiescenceGrace)
		evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvOutputActivity})
		// If input was recent (within 5 seconds), output corroborates ongoing working turn
		if !s.lastInput.IsZero() && at.Sub(s.lastInput) < 5*time.Second {
			evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvInput, Origin: ptyevent.OriginHuman})
		}

	case ptyevent.KindInput:
		// Active input resets quiescence timer
		s.lastActivity = at
		s.lastInput = at
		s.quiescence.Reset(c.cfg.QuiescenceGrace)
		orig := ptyevent.OriginHuman
		if ev.Input != nil && ev.Input.Origin.Valid() {
			orig = ev.Input.Origin
		}
		evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvInput, Origin: orig})

	case ptyevent.KindLifecycle:
		if ev.Lifecycle != nil {
			if ev.Lifecycle.PID > 0 {
				s.rootPID = ev.Lifecycle.PID
			}
			switch ev.Lifecycle.Phase {
			case ptyevent.PhaseSpawned, ptyevent.PhaseRecovered:
				evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvProcessSpawn})
			case ptyevent.PhaseReady:
				evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvReady})
			case ptyevent.PhaseExited, ptyevent.PhaseSignaled, ptyevent.PhaseCrashed:
				evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvProcessExit})
				s.quiescence.Stop()
			}
		}

	case ptyevent.KindState:
		// Update snapshot if already emitted externally
		if ev.State != nil {
			s.snapshot.State = ev.State.To
			s.snapshot.Reason = ev.State.Reason
			s.snapshot.Since = at
		}
		return
	default:
		return
	}

	if len(evs) > 0 {
		obs := ptyevent.Observation{
			At:       at,
			Seq:      ev.Seq,
			Evidence: evs,
		}
		c.stepSession(s, obs)
	}
}

// OnHook handles backend lifecycle hook events (strong signals).
func (c *Classifier) OnHook(name string, state ptyevent.State, reason ptyevent.WaitingReason) {
	s := c.getOrCreate(name)
	s.mu.Lock()
	defer s.mu.Unlock()

	obs := ptyevent.Observation{
		At:  time.Now(),
		Seq: 0,
		Evidence: []ptyevent.Evidence{
			{Kind: ptyevent.EvHook, Hook: state, Reason: reason},
		},
	}
	c.stepSession(s, obs)
}

// onQuiescence is triggered when terminal silence exceeds QuiescenceGrace.
// It never asserts a state from silence alone; it gathers corroborating evidence (Investigation).
func (c *Classifier) onQuiescence(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.InvestigationTimeout)
	defer cancel()
	c.Investigate(ctx, name)
}

// Investigate gathers corroborating evidence across screen, processes, transcripts,
// and folds it through the state machine.
func (c *Classifier) Investigate(ctx context.Context, name string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	s := c.getOrCreate(name)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snapshot.State == ptyevent.StateExited {
		return
	}

	target, hasTarget := c.mgr.Lookup(name)
	var (
		screen  string
		seq     uint64
		rootPID = s.rootPID
	)

	if hasTarget && target.Stream != nil {
		screen = target.Stream.Screen()
		if j := target.Stream.Journal(); j != nil {
			seq = j.NextSeq()
		}
		if target.Direct != nil && target.Direct.PID() > 0 {
			rootPID = target.Direct.PID()
			s.rootPID = rootPID
		}
	}

	now := time.Now()
	quiet := now.Sub(s.lastActivity)
	var evs []ptyevent.Evidence

	// 1. Process tree inspection
	if rootPID > 0 && c.procs != nil {
		pinfo, err := c.procs.Inspect(ctx, rootPID)
		if err == nil {
			if !pinfo.RootAlive {
				evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvProcessExit})
			} else if pinfo.HasActiveChildren && screen == "" {
				// Subprocess is working without CLI output
				evs = append(evs, ptyevent.Evidence{
					Kind:   ptyevent.EvLimitBanner,
					Reason: ptyevent.ReasonSubprocess,
				})
			}
		}
	}

	// 2. Screen & backend state detection
	backend := c.backendFor(name)
	if ag := c.agentRecord(name); ag != nil {
		s.workdir = ag.Workdir
	}
	if screen != "" && backend != nil {
		st := backend.DetectState(screen)
		switch st {
		case agentbackend.StateWorking:
			evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvWorkingMarker})
		case agentbackend.StateIdle:
			evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvIdleMarker})
		case agentbackend.StateNeedsInput:
			app, ok := backend.ParseApproval(screen)
			if ok && app != nil {
				prompt, err := decisionrouter.NormalizeApproval(backend, app, decisionrouter.NormalizeContext{
					BackendID:         backend.ID(),
					Screen:            screen,
					Workdir:           s.workdir,
					TargetInWorkspace: s.workdir == "",
				})
				if err == nil && prompt != nil && prompt.Validate() == nil {
					if s.currentPrompt == nil || s.currentPrompt.InstanceKey != prompt.InstanceKey {
						s.currentPrompt = prompt
						c.emitPrompt(target, ptyevent.PromptObserved, prompt)
					}
					evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvPrompt, Prompt: prompt})
					if c.OnPrompt != nil {
						c.OnPrompt(name, prompt)
					}
				} else {
					evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvUncertain})
					if c.OnUnreadable != nil {
						c.OnUnreadable(name, screen)
					}
				}
			} else {
				evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvUncertain})
				if c.OnUnreadable != nil {
					c.OnUnreadable(name, screen)
				}
			}
		}

		// Rate limit banner detection
		if rl, ok := backend.(agentbackend.RateLimitDetector); ok {
			if limited, _, _ := rl.DetectRateLimit(screen); limited {
				evs = append(evs, ptyevent.Evidence{
					Kind:   ptyevent.EvLimitBanner,
					Reason: ptyevent.ReasonRateLimit,
				})
			}
		}
	}

	// 3. Prompt clearing check
	if s.currentPrompt != nil {
		hasPrompt := false
		for _, e := range evs {
			if e.Kind == ptyevent.EvPrompt {
				hasPrompt = true
				break
			}
		}
		if !hasPrompt {
			c.emitPrompt(target, ptyevent.PromptCleared, s.currentPrompt)
			s.currentPrompt = nil
			evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvPromptCleared})
		}
	}

	// 4. Transcript inspection (best effort)
	if c.transcripts != nil && backend != nil {
		ag := c.agentRecord(name)
		if ag != nil {
			tinfo, err := c.transcripts.InspectTranscript(ctx, ag.Workdir, ag.AICLISessionID, backend)
			if err == nil {
				if tinfo.TurnEnded {
					evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvTurnEnd})
				}
				if tinfo.TurnActive {
					evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvTurnStart})
				}
			}
		}
	}

	// 5. Silence duration (never sufficient alone, but corroborates idle_marker or held state)
	evs = append(evs, ptyevent.Evidence{Kind: ptyevent.EvSilence, Quiet: quiet})

	obs := ptyevent.Observation{
		At:       now,
		Seq:      seq,
		Evidence: evs,
	}
	changed := c.stepSession(s, obs)
	c.countInvestigation(name, evs, changed, s.snapshot.State)

	// Reschedule next investigation if still quiet and not exited
	if s.snapshot.State != ptyevent.StateExited {
		s.quiescence.Reset(c.cfg.QuiescenceGrace)
	}
}

func (c *Classifier) stepSession(s *sessionState, obs ptyevent.Observation) bool {
	next, ch := ptyevent.Step(s.snapshot, obs, c.cfg.Hysteresis)
	s.snapshot = next

	if ch == nil {
		return false
	}

	// Explainable evidence trail
	rec := TransitionRecord{
		At:       obs.At,
		From:     ch.From,
		To:       ch.To,
		Reason:   ch.Reason,
		Evidence: ch.Evidence,
		Seq:      obs.Seq,
	}
	s.trail = append(s.trail, rec)
	if len(s.trail) > maxTrail {
		s.trail = append(s.trail[:0], s.trail[len(s.trail)-maxTrail:]...)
	}
	c.countTransition(s, obs, ch)

	slog.Info("stateclassifier: committed transition",
		"session", s.name,
		"from", ch.From,
		"to", ch.To,
		"reason", ch.Reason,
		"evidence", ch.Evidence,
	)

	// Journal KindState event to stream
	if target, ok := c.mgr.Lookup(s.name); ok && target.Stream != nil {
		ev := ptyevent.Event{
			Kind:  ptyevent.KindState,
			State: ch,
		}
		_, _ = target.Stream.Emit(ev)
	}

	// Update store status if authoritative (mode on)
	if c.mgr.IsAuthoritative(s.name) {
		if storeSt, ok := ToStoreStatus(ch.To, ch.Reason); ok && c.store != nil {
			ag := c.agentRecord(s.name)
			if ag != nil {
				_, _ = c.store.UpdateStatusIf(context.Background(), ag.ID, ag.Status, storeSt)
			}
		}
	} else if c.mgr.Mode() == ptyhost.ModeShadow {
		// Shadow mode: log disagreements without mutating store
		if storeSt, ok := ToStoreStatus(ch.To, ch.Reason); ok {
			ag := c.agentRecord(s.name)
			if ag != nil && ag.Status != storeSt {
				slog.Info("stateclassifier[shadow]: disagreement with poller",
					"session", s.name,
					"classifier_state", ch.To,
					"expected_status", storeSt,
					"store_status", ag.Status,
					"evidence", ch.Evidence,
				)
				if c.OnDisagreement != nil {
					c.OnDisagreement(s.name, ch.To, ag.Status)
				}
			}
		}
	}

	if c.OnTransition != nil {
		c.OnTransition(s.name, ch)
	}
	return true
}

func (c *Classifier) emitPrompt(target ptyhost.Target, phase ptyevent.PromptPhase, p *ptyevent.Prompt) {
	if target.Stream == nil || p == nil {
		return
	}
	ev := ptyevent.Event{
		Kind: ptyevent.KindPrompt,
		Prompt: &ptyevent.PromptEvent{
			Phase:       phase,
			Prompt:      p,
			Fingerprint: p.Fingerprint,
		},
	}
	_, _ = target.Stream.Emit(ev)
}

func (c *Classifier) backendFor(name string) agentbackend.Backend {
	if c.backends == nil {
		return nil
	}
	ag := c.agentRecord(name)
	if ag == nil {
		return nil
	}
	return c.backends(ag)
}

func (c *Classifier) agentRecord(name string) *agentstore.Agent {
	if c.store == nil {
		return nil
	}
	ag, err := c.store.Get(context.Background(), name)
	if err == nil {
		return ag
	}
	return nil
}

// Close stops all per-session timers.
func (c *Classifier) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, s := range c.sessions {
		s.mu.Lock()
		if s.quiescence != nil {
			s.quiescence.Stop()
		}
		s.mu.Unlock()
	}
}
