package fastbrain

// Central admission controller (design: docs/specs/2026-10-10-fast-brain-
// decision-inventory-and-slos.md §4, §5). Every Decide call passes through it
// before any runner is invoked. It evolves the #858 semaphore + exact in-flight
// coalescing into one component providing:
//
//   - priority classes (1 safety … 4 best-effort) with a reserved slot for P1;
//   - bounded per-class queues with explicit backpressure (StatusDeferred) and
//     per-class maximum queue wait; P4 is never queued, only shed;
//   - per-agent fairness (P2–P4) and per-kind concurrency limits;
//   - sanitized decision-identity dedup whose shared call is owned by the
//     engine, not by the first caller (followers survive a leader's cancel);
//   - a small bounded result cache for pure-content kinds (success and short
//     terminal-failure entries);
//   - preemption of a running P4 call when a P1/P2 call is blocked.
//
// The controller never sees terminal content beyond the prompt it hashes; the
// cache key is a SHA-256 of kind, tier and the sanitized prompt, and no prompt,
// agent id or error text is ever used as a metric label.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

// Class is a decision's admission priority; lower is more important.
type Class int

const (
	ClassSafety      Class = 1 // permission/trust prompts, safety decisions
	ClassInteractive Class = 2 // human-interactive requests, bounded Autopilot recovery
	ClassOperational Class = 3 // operational summaries, check diagnosis
	ClassBestEffort  Class = 4 // badges, naming, narration, curation, insights
)

// ClassOf returns the admission class of kind. Unknown kinds are operational:
// they are neither starved behind cosmetics nor allowed to crowd out safety.
func ClassOf(kind DecisionKind) Class {
	switch kind {
	case KindArbitrateApproval, KindRecognizePrompt:
		return ClassSafety
	case KindReplTurn, KindDiagnoseStall:
		return ClassInteractive
	case KindClassifyTask, KindResolveAgentName, KindSummarizeActivity, KindCurateExtract:
		return ClassBestEffort
	default:
		return ClassOperational
	}
}

// Default admission bounds.
const (
	DefaultQueueDepth      = 16
	DefaultCacheTTL        = 2 * time.Minute
	DefaultFailureTTL      = 15 * time.Second
	DefaultCacheEntries    = 128
	maxCachedOutputBytes   = 64 << 10
	defaultInteractiveWait = 3 * time.Second
	defaultOperationalWait = 5 * time.Second
)

// AdmissionOptions tunes the controller. The zero value yields the documented
// defaults; every bound is finite.
type AdmissionOptions struct {
	// QueueDepth bounds each class's waiting queue (default 16; 64 for P1). A full queue
	// sheds the new call with StatusDeferred (explicit backpressure).
	QueueDepth int
	// QueueWait overrides the maximum time a call may wait for a slot, per
	// class. Defaults: P1 unbounded (caller context only), P2 3 s, P3 5 s, P4
	// 0 (shed, never queued). A zero entry keeps the default.
	QueueWait map[Class]time.Duration
	// KindLimits caps concurrent runner invocations per kind (0 = default:
	// 1 for P4 kinds, unlimited otherwise).
	KindLimits map[DecisionKind]int
	// CacheTTL is how long a successful result of a cacheable kind is reused
	// (default 2 m; negative disables the cache). FailureTTL does the same for
	// terminal failures (default 15 s).
	CacheTTL, FailureTTL time.Duration
	// CacheEntries bounds the cache (default 128).
	CacheEntries int
}

// AdmissionInfo describes how admission treated one Decide call. It carries no
// prompt or agent identity.
type AdmissionInfo struct {
	Class     Class
	QueueWait time.Duration
	Coalesced bool   // joined another caller's in-flight call
	Cached    bool   // served from the result cache; no runner invoked
	Reason    string // bounded vocabulary, set when the call was shed
}

// AdmissionSnapshot is a point-in-time, content-free view of the controller
// for operator surfaces and tests.
type AdmissionSnapshot struct {
	Active     int
	ActiveBy   map[Class]int
	Queued     map[Class]int
	Admitted   uint64
	Coalesced  uint64
	CacheHits  uint64
	Preempted  uint64
	Shed       map[string]uint64 // reason → count
	CacheItems int
}

// Shed reasons (bounded vocabulary).
const (
	ShedReservedCapacity = "reserved_capacity"
	ShedQueueFull        = "queue_full"
	ShedQueueWait        = "queue_wait_exceeded"
	ShedPreempted        = "preempted"
)

type admCall struct {
	key      string
	kind     DecisionKind
	tier     Tier
	class    Class
	agent    string
	ctx      context.Context
	cancel   context.CancelFunc
	refs     int
	created  time.Time
	admitted chan struct{} // closed when a slot is granted
	started  bool
	finished bool
	preempt  bool
	done     chan struct{}
	resp     Response
	hash     string
}

type cacheEntry struct {
	resp    Response
	expires time.Time
}

type admission struct {
	mu       sync.Mutex
	max      int
	opts     AdmissionOptions
	active   int
	byClass  [5]int
	byKind   map[DecisionKind]int
	byAgent  map[string]int
	queues   [5][]*admCall
	running  map[*admCall]struct{}
	inflight map[string]*admCall
	cache    map[string]cacheEntry
	order    []string
	stats    AdmissionSnapshot
}

func newAdmission(max int, o AdmissionOptions) *admission {
	if o.CacheTTL == 0 {
		o.CacheTTL = DefaultCacheTTL
	}
	if o.FailureTTL == 0 {
		o.FailureTTL = DefaultFailureTTL
	}
	if o.CacheEntries <= 0 {
		o.CacheEntries = DefaultCacheEntries
	}
	return &admission{
		max: max, opts: o,
		byKind: map[DecisionKind]int{}, byAgent: map[string]int{},
		running: map[*admCall]struct{}{}, inflight: map[string]*admCall{},
		cache: map[string]cacheEntry{},
		stats: AdmissionSnapshot{Shed: map[string]uint64{}},
	}
}

func (a *admission) queueWait(c Class) time.Duration {
	if d := a.opts.QueueWait[c]; d > 0 {
		return d
	}
	switch c {
	case ClassInteractive:
		return defaultInteractiveWait
	case ClassOperational:
		return defaultOperationalWait
	}
	return 0 // P1: caller context only; P4: never queued
}

// queueDepth bounds a class's queue. Safety decisions get 4x headroom so they
// are shed only under a truly extreme flood.
func (a *admission) queueDepth(c Class) int {
	d := a.opts.QueueDepth
	if d > 0 {
		return d
	}
	if c == ClassSafety {
		return 4 * DefaultQueueDepth
	}
	return DefaultQueueDepth
}

func (a *admission) kindLimit(k DecisionKind) int {
	if n, ok := a.opts.KindLimits[k]; ok {
		return n
	}
	if ClassOf(k) == ClassBestEffort {
		return 1
	}
	return 0
}

func (a *admission) agentCap() int { return (a.max + 1) / 2 }

// canStart reports whether c may take a slot now. Caller holds a.mu.
func (a *admission) canStart(c *admCall) bool {
	if a.active >= a.max {
		return false
	}
	if lim := a.kindLimit(c.kind); lim > 0 && a.byKind[c.kind] >= lim {
		return false
	}
	if c.class == ClassSafety {
		return true
	}
	// P1 owns a reserved slot: non-P1 work never fills the pool.
	capNonP1 := max(1, a.max-1)
	if a.active-a.byClass[ClassSafety] >= capNonP1 {
		return false
	}
	// Generalised #858 rule: best-effort work needs a slot left for P1–P2.
	if c.class == ClassBestEffort && a.active >= a.max-1 {
		return false
	}
	if c.agent != "" && a.byAgent[c.agent] >= a.agentCap() {
		return false
	}
	return true
}

// dispatch grants slots to queued calls in priority order, fairly across
// agents within a class, and preempts a running P4 call if P1/P2 is blocked.
// Caller holds a.mu.
func (a *admission) dispatch() {
	for {
		started := false
		for cl := ClassSafety; cl <= ClassBestEffort; cl++ {
			best := -1
			for i, c := range a.queues[cl] {
				if !a.canStart(c) {
					continue
				}
				if best < 0 || a.agentLoad(c) < a.agentLoad(a.queues[cl][best]) {
					best = i
				}
			}
			if best >= 0 {
				c := a.queues[cl][best]
				a.queues[cl] = append(a.queues[cl][:best], a.queues[cl][best+1:]...)
				a.start(c)
				started = true
				break
			}
		}
		if !started {
			break
		}
	}
	if !a.slotBlocked() {
		return
	}
	for c := range a.running {
		if c.class == ClassBestEffort && !c.preempt {
			c.preempt = true
			a.stats.Preempted++
			c.cancel()
			return
		}
	}
}

// slotBlocked reports whether a queued P1/P2 call is waiting for capacity
// (rather than for its agent's fairness cap).
func (a *admission) slotBlocked() bool {
	for cl := ClassSafety; cl <= ClassInteractive; cl++ {
		for _, c := range a.queues[cl] {
			if c.agent == "" || a.byAgent[c.agent] < a.agentCap() {
				return true
			}
		}
	}
	return false
}

func (a *admission) agentLoad(c *admCall) int {
	if c.agent == "" {
		return 0
	}
	return a.byAgent[c.agent]
}

func (a *admission) start(c *admCall) {
	c.started = true
	a.active++
	a.byClass[c.class]++
	a.byKind[c.kind]++
	if c.agent != "" {
		a.byAgent[c.agent]++
	}
	a.running[c] = struct{}{}
	a.stats.Admitted++
	close(c.admitted)
}

func (a *admission) release(c *admCall) {
	a.active--
	a.byClass[c.class]--
	a.byKind[c.kind]--
	if c.agent != "" {
		if a.byAgent[c.agent]--; a.byAgent[c.agent] <= 0 {
			delete(a.byAgent, c.agent)
		}
	}
	delete(a.running, c)
}

func (a *admission) dequeue(c *admCall) {
	q := a.queues[c.class]
	for i, x := range q {
		if x == c {
			a.queues[c.class] = append(q[:i], q[i+1:]...)
			return
		}
	}
}

func (a *admission) shed(c Class, reason string, req Request, detail string) Response {
	a.stats.Shed[reason]++
	return Response{
		Kind: req.Kind, Tier: req.Tier, Status: StatusDeferred,
		Error:     "admission: " + detail,
		Admission: AdmissionInfo{Class: c, Reason: reason},
	}
}

// admissionKey is the decision identity: kind, tier and the sanitized prompt.
func admissionKey(req Request) string {
	h := sha256.New()
	h.Write([]byte(string(req.Kind) + "\x00" + string(req.Tier) + "\x00"))
	h.Write([]byte(Sanitize(req.Prompt)))
	return hex.EncodeToString(h.Sum(nil))
}

func agentOf(req Request) string {
	if id := req.Metadata["agent_id"]; id != "" {
		return id
	}
	return req.Metadata["run_id"]
}

// cacheable kinds are pure functions of their prompt: re-asking within the TTL
// cannot change the answer's validity. Approval, recognition, REPL, stall
// triage (state-dependent) and naming (should vary) are never cached.
func cacheable(k DecisionKind) bool {
	switch k {
	case KindClassifyTask, KindSummarizeActivity, KindCurateExtract, KindCommitMessage,
		KindPRSummary, KindSummarizeCheck, KindClassifyCIFailure, KindDiagnoseFailure, KindRouteTier:
		return true
	}
	return false
}

func cloneResp(r Response) Response {
	if r.Output.Parsed != nil {
		r.Output.Parsed = append([]byte(nil), r.Output.Parsed...)
	}
	return r
}

func (a *admission) cacheGet(key string, now time.Time) (Response, bool) {
	e, ok := a.cache[key]
	if !ok {
		return Response{}, false
	}
	if now.After(e.expires) {
		delete(a.cache, key)
		return Response{}, false
	}
	return cloneResp(e.resp), true
}

func (a *admission) cachePut(key string, r Response, now time.Time) {
	var ttl time.Duration
	switch r.Status {
	case StatusOK:
		ttl = a.opts.CacheTTL
	case StatusInvalidJSON, StatusRunnerError, StatusTimeout:
		ttl = a.opts.FailureTTL
	}
	if ttl <= 0 || len(r.Output.Raw) > maxCachedOutputBytes {
		return
	}
	if _, ok := a.cache[key]; !ok {
		a.order = append(a.order, key)
		for len(a.order) > a.opts.CacheEntries {
			delete(a.cache, a.order[0])
			a.order = a.order[1:]
		}
	}
	a.cache[key] = cacheEntry{resp: cloneResp(r), expires: now.Add(ttl)}
}

// Snapshot returns a copy of the controller's counters and gauges.
func (a *admission) Snapshot() AdmissionSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stats
	s.Active = a.active
	s.ActiveBy = map[Class]int{}
	s.Queued = map[Class]int{}
	for c := ClassSafety; c <= ClassBestEffort; c++ {
		s.ActiveBy[c] = a.byClass[c]
		s.Queued[c] = len(a.queues[c])
	}
	s.Shed = map[string]uint64{}
	for k, v := range a.stats.Shed {
		s.Shed[k] = v
	}
	s.CacheItems = len(a.cache)
	return s
}

// submit admits req and returns its Response. exec performs the runner call
// (with the budget applied) once a slot is held.
func (a *admission) submit(ctx context.Context, req Request, exec func(context.Context) Response) Response {
	class := ClassOf(req.Kind)
	key := admissionKey(req)
	now := time.Now()

	a.mu.Lock()
	if cacheable(req.Kind) && a.opts.CacheTTL > 0 {
		if r, ok := a.cacheGet(key, now); ok {
			a.stats.CacheHits++
			a.mu.Unlock()
			r.Admission = AdmissionInfo{Class: class, Cached: true}
			return r
		}
	}
	if c := a.inflight[key]; c != nil {
		c.refs++
		a.stats.Coalesced++
		if class < c.class && !c.started {
			// A more important caller joined a queued call: promote it.
			a.dequeue(c)
			c.class = class
			a.queues[class] = append(a.queues[class], c)
			a.dispatch()
		}
		a.mu.Unlock()
		r := a.wait(ctx, c, req, class)
		r.Admission.Coalesced = true
		return r
	}
	if class != ClassBestEffort && len(a.queues[class]) >= a.queueDepth(class) {
		r := a.shed(class, ShedQueueFull, req, "queue full")
		a.mu.Unlock()
		return r
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &admCall{
		key: key, kind: req.Kind, tier: req.Tier, class: class, agent: agentOf(req),
		ctx: cctx, cancel: cancel, refs: 1, created: now,
		admitted: make(chan struct{}), done: make(chan struct{}),
		hash: promptHash(req.Prompt),
	}
	a.queues[class] = append(a.queues[class], c)
	a.dispatch()
	if !c.started && class == ClassBestEffort {
		a.dequeue(c)
		r := a.shed(class, ShedReservedCapacity, req, "reserved capacity for operational decisions")
		a.mu.Unlock()
		cancel()
		return r
	}
	a.inflight[key] = c
	a.mu.Unlock()

	go a.drive(c, req, exec)
	return a.wait(ctx, c, req, class)
}

// wait blocks until the shared call finishes or this caller's context ends. A
// departing caller only detaches; the call is cancelled when none remain.
func (a *admission) wait(ctx context.Context, c *admCall, req Request, class Class) Response {
	select {
	case <-c.done:
		return cloneResp(c.resp)
	default:
	}
	select {
	case <-c.done:
		return cloneResp(c.resp)
	case <-ctx.Done():
		a.mu.Lock()
		if !c.finished {
			if c.refs--; c.refs == 0 {
				if a.inflight[c.key] == c {
					delete(a.inflight, c.key)
				}
				c.cancel()
			}
		}
		a.mu.Unlock()
		return Response{
			Kind: req.Kind, Tier: req.Tier, Status: StatusCanceled, Error: ctx.Err().Error(),
			Admission: AdmissionInfo{Class: class},
		}
	}
}

// drive owns the shared call: wait for a slot, run, release, record.
func (a *admission) drive(c *admCall, req Request, exec func(context.Context) Response) {
	var timeout <-chan time.Time
	if w := a.queueWait(c.class); w > 0 {
		t := time.NewTimer(w)
		defer t.Stop()
		timeout = t.C
	}
	var resp Response
	ran := false
	select {
	case <-c.admitted:
		ran = true
	case <-c.ctx.Done():
	case <-timeout:
	}
	a.mu.Lock()
	if !ran && c.started { // slot granted in the same instant
		ran = true
	}
	if !ran {
		a.dequeue(c)
		a.mu.Unlock()
		if c.ctx.Err() != nil {
			resp = Response{Kind: req.Kind, Tier: req.Tier, Status: StatusCanceled, Error: c.ctx.Err().Error()}
		} else {
			a.mu.Lock()
			resp = a.shed(c.class, ShedQueueWait, req, "queue wait exceeded")
			a.mu.Unlock()
		}
		resp.Admission.Class = c.class
		resp.Admission.QueueWait = time.Since(c.created)
		resp.Duration = resp.Admission.QueueWait
		a.finish(c, resp, false)
		return
	}
	a.mu.Unlock()

	queueWait := time.Since(c.created)
	resp = exec(c.ctx)
	a.mu.Lock()
	a.release(c)
	if c.preempt && resp.Status == StatusCanceled {
		resp.Status, resp.Error = StatusDeferred, "admission: preempted by a higher-priority decision"
		resp.Admission.Reason = ShedPreempted
		a.stats.Shed[ShedPreempted]++
	}
	resp.Admission.Class = c.class
	resp.Admission.QueueWait = queueWait
	resp.Duration = time.Since(c.created)
	a.mu.Unlock()
	slog.Info("fastbrain decide",
		"kind", req.Kind, "tier", req.Tier, "class", int(c.class),
		"queue_wait", queueWait, "duration", resp.Duration,
		"status", resp.Status, "prompt_hash", c.hash)
	a.finish(c, resp, true)
}

func (a *admission) finish(c *admCall, resp Response, ran bool) {
	a.mu.Lock()
	if ran && cacheable(c.kind) && a.opts.CacheTTL > 0 && c.ctx.Err() == nil {
		a.cachePut(c.key, resp, time.Now())
	}
	if a.inflight[c.key] == c {
		delete(a.inflight, c.key)
	}
	c.resp = resp
	c.finished = true
	close(c.done)
	c.cancel()
	a.dispatch()
	a.mu.Unlock()
}
