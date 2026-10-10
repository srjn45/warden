package fastbrain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Default per-tier ceilings. They suit a fresh native AI CLI process; callers
// may configure different ceilings through EngineOptions.
const (
	FastTimeout     = 10 * time.Second
	ThinkingTimeout = 20 * time.Second
)

// EngineOptions bounds the daemon-wide internal decision workload. MaxConcurrent
// is deliberately small because each runner may start a heavyweight native CLI.
type EngineOptions struct {
	FastTimeout     time.Duration
	ThinkingTimeout time.Duration
	MaxConcurrent   int
	// CancelGrace is how long the engine waits for a runner to return after
	// its context ended before abandoning it. Zero means 500 ms.
	CancelGrace time.Duration
	// Admission tunes queues, fairness and the result cache.
	Admission AdmissionOptions
	// Audit receives bounded operator-facing events (control changes, circuit
	// transitions, abandoned runners). Nil disables them.
	Audit AuditFunc
	// HealthSnapshot supplies runner circuit state for telemetry. Optional.
	HealthSnapshot func() []RunnerHealth
	// PausedKinds are paused from start (config-driven, never auto-expire).
	PausedKinds []DecisionKind
}

func (o EngineOptions) normalized() EngineOptions {
	if o.FastTimeout <= 0 {
		o.FastTimeout = FastTimeout
	}
	if o.ThinkingTimeout <= 0 {
		o.ThinkingTimeout = ThinkingTimeout
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 2
	}
	if o.CancelGrace <= 0 {
		o.CancelGrace = 500 * time.Millisecond
	}
	return o
}

// Runner performs one headless model completion. It deliberately matches
// agentname.BackendRunner so a single runner can serve both.
type Runner interface {
	Run(ctx context.Context, prompt string) (string, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, prompt string) (string, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, prompt string) (string, error) { return f(ctx, prompt) }

type engine struct {
	fast, thinking Runner
	opts           EngineOptions
	adm            *admission
	tel            *telemetry
	abandoned      atomic.Int64 // runner calls abandoned after ignoring ctx (total)
	abandonedLive  atomic.Int64 // abandoned calls whose goroutine is still running
}

// CancelStats reports runner calls abandoned because they ignored their
// context. Live > 0 means a runner is still holding a goroutine/process.
type CancelStats struct {
	Abandoned int64
	Live      int64
}

// CancelStats returns the engine's abandoned-runner counters.
func (e *engine) CancelStats() CancelStats {
	return CancelStats{Abandoned: e.abandoned.Load(), Live: e.abandonedLive.Load()}
}

// NewEngine returns the Engine routing TierFast/TierThinking to the given
// runners. Either may be nil; calls to a nil tier fail open.
func NewEngine(fastRunner, thinkingRunner Runner) Engine {
	return NewEngineWithOptions(fastRunner, thinkingRunner, EngineOptions{})
}

// NewEngineWithOptions constructs an engine whose every call is admitted by the
// central admission controller (admission.go): priority classes, bounded
// queues, fairness, identity dedup and a small result cache.
func NewEngineWithOptions(fastRunner, thinkingRunner Runner, opts EngineOptions) Engine {
	opts = opts.normalized()
	e := &engine{
		fast: fastRunner, thinking: thinkingRunner, opts: opts,
		adm: newAdmission(opts.MaxConcurrent, opts.Admission),
		tel: newTelemetry(opts.Audit, opts.HealthSnapshot, nil),
	}
	for _, k := range opts.PausedKinds {
		_ = e.tel.setPaused(k, true, 0, "config")
	}
	return e
}

// Inspector exposes redacted telemetry and per-kind operator controls. The
// daemon engine implements it; test doubles need not.
type Inspector interface {
	Telemetry() TelemetrySnapshot
	Decisions(limit int) []Decision
	SetKindPaused(kind DecisionKind, paused bool, ttl time.Duration) error
}

// ErrUnknownKind is returned when a control names a kind the engine does not
// know.
var ErrUnknownKind = errors.New("fastbrain: unknown decision kind")

// Telemetry returns the content-free operator snapshot (spec §8).
func (e *engine) Telemetry() TelemetrySnapshot {
	s := e.tel.snapshot()
	a := e.adm.Snapshot()
	s.MaxConcurrent, s.Active = e.opts.MaxConcurrent, a.Active
	s.Admitted, s.Coalesced, s.CacheHits, s.Preempted = a.Admitted, a.Coalesced, a.CacheHits, a.Preempted
	s.CacheItems, s.Shed = a.CacheItems, a.Shed
	for c := ClassSafety; c <= ClassBestEffort; c++ {
		s.Queues = append(s.Queues, QueueDepth{Class: c, Active: a.ActiveBy[c], Queued: a.Queued[c]})
	}
	cs := e.CancelStats()
	s.Abandoned, s.AbandonedLive = cs.Abandoned, cs.Live
	return s
}

// Decisions returns the most recent redacted decision traces, newest first.
func (e *engine) Decisions(limit int) []Decision { return e.tel.decisions(limit) }

// SetKindPaused pauses (bounded ttl) or resumes one decision kind. A paused
// kind fails open without invoking a runner.
func (e *engine) SetKindPaused(kind DecisionKind, paused bool, ttl time.Duration) error {
	return e.tel.setPaused(kind, paused, ttl, "operator")
}

// AdmissionSnapshot reports the controller's content-free counters and gauges.
func (e *engine) AdmissionSnapshot() AdmissionSnapshot { return e.adm.Snapshot() }

func (e *engine) Decide(ctx context.Context, req Request) (Response, error) {
	resp, err := e.decide(ctx, req)
	if err == nil {
		e.tel.observe(resp)
	}
	return resp, err
}

func (e *engine) decide(ctx context.Context, req Request) (Response, error) {
	if req.Kind == "" {
		return Response{}, fmt.Errorf("%w: empty kind", ErrInvalidRequest)
	}
	if req.Prompt == "" {
		return Response{}, fmt.Errorf("%w: empty prompt", ErrInvalidRequest)
	}
	var runner Runner
	var budget time.Duration
	switch req.Tier {
	case TierFast:
		runner, budget = e.fast, e.opts.FastTimeout
	case TierThinking:
		runner, budget = e.thinking, e.opts.ThinkingTimeout
	default:
		return Response{}, fmt.Errorf("%w: unknown tier %q", ErrInvalidRequest, req.Tier)
	}
	if req.Timeout > 0 && req.Timeout < budget {
		budget = req.Timeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if e.tel.paused(req.Kind) {
		return Response{
			Kind: req.Kind, Tier: req.Tier, Status: StatusDeferred, Error: "kind paused by operator",
			Admission: AdmissionInfo{Class: ClassOf(req.Kind), Reason: ShedPaused},
		}, nil
	}
	if runner == nil {
		return Response{
			Kind: req.Kind, Tier: req.Tier, Status: StatusNoRunner, Error: "no runner configured for tier",
			Admission: AdmissionInfo{Class: ClassOf(req.Kind)},
		}, nil
	}
	return e.adm.submit(ctx, req, func(cctx context.Context) Response {
		return e.execute(cctx, req, runner, budget)
	}), nil
}

// execute runs one admitted call. ctx is owned by the admission controller and
// ends only when every interested caller is gone (or on preemption).
func (e *engine) execute(ctx context.Context, req Request, runner Runner, budget time.Duration) Response {
	resp := Response{Kind: req.Kind, Tier: req.Tier}

	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	raw, sel, cancelOutcome, err := e.invoke(rctx, runner, req.Prompt)
	resp.Output.Raw = raw
	resp.Selection, resp.Cancel = sel, cancelOutcome
	if err != nil {
		switch {
		case errors.Is(err, ErrNoCandidate):
			resp.Status = StatusNoRunner
		case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
			resp.Status = StatusCanceled
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(rctx.Err(), context.DeadlineExceeded):
			resp.Status = StatusTimeout
		default:
			resp.Status = StatusRunnerError
		}
		resp.Error = err.Error()
		return resp
	}
	// A runner that ignored ctx may return late; honour the deadline anyway.
	if rctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			resp.Status = StatusCanceled
		} else {
			resp.Status = StatusTimeout
		}
		resp.Error = rctx.Err().Error()
		return resp
	}

	obj, err := SanitizeJSON(raw)
	if err != nil {
		resp.Status, resp.Error = StatusInvalidJSON, err.Error()
		return resp
	}
	resp.Output.Parsed = obj
	resp.Confidence, resp.Rationale = extractMeta(obj)
	resp.Status = StatusOK
	return resp
}

// invoke runs the runner, returning when it finishes or, if it ignores ctx,
// after the cancel grace period so a context-ignoring runner can neither pin an
// admission slot nor stall the caller past its deadline. The returned
// CancelOutcome records whether the stop was acknowledged or abandoned.
func (e *engine) invoke(ctx context.Context, runner Runner, prompt string) (string, Selection, CancelOutcome, error) {
	type result struct {
		raw string
		sel Selection
		err error
	}
	ch := make(chan result, 1)
	var state atomic.Int32 // 0 running, 1 finished, 2 abandoned by the engine
	go func() {
		var r result
		if dr, ok := runner.(DetailedRunner); ok {
			r.raw, r.sel, r.err = dr.RunDetailed(ctx, prompt)
		} else {
			r.raw, r.err = runner.Run(ctx, prompt)
		}
		if !state.CompareAndSwap(0, 1) {
			e.abandonedLive.Add(-1)
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		if ctx.Err() != nil {
			return r.raw, r.sel, CancelAcknowledged, r.err
		}
		return r.raw, r.sel, CancelNone, r.err
	case <-ctx.Done():
	}
	timer := time.NewTimer(e.opts.CancelGrace)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.raw, r.sel, CancelAcknowledged, r.err
	case <-timer.C:
	}
	if !state.CompareAndSwap(0, 2) {
		// Finished just as the grace period ended.
		r := <-ch
		return r.raw, r.sel, CancelAcknowledged, r.err
	}
	e.abandonedLive.Add(1)
	e.abandoned.Add(1)
	slog.Warn("fastbrain runner ignored cancellation; abandoned", "grace", e.opts.CancelGrace)
	return "", Selection{}, CancelAbandoned, ctx.Err()
}

// promptHash is a short, non-reversible fingerprint so logs can correlate
// prompts without ever containing their text (which may hold credentials).
func promptHash(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:4])
}
