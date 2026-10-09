package fastbrain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	sem            chan struct{}
	mu             sync.Mutex
	active         int
	inflight       map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	resp Response
}

// NewEngine returns the Engine routing TierFast/TierThinking to the given
// runners. Either may be nil; calls to a nil tier fail open.
func NewEngine(fastRunner, thinkingRunner Runner) Engine {
	return NewEngineWithOptions(fastRunner, thinkingRunner, EngineOptions{})
}

// NewEngineWithOptions constructs a bounded, in-flight-coalescing engine.
// Identical requests share one runner invocation; callers remain independently
// cancellable while waiting for that result.
func NewEngineWithOptions(fastRunner, thinkingRunner Runner, opts EngineOptions) Engine {
	opts = opts.normalized()
	return &engine{
		fast: fastRunner, thinking: thinkingRunner, opts: opts,
		sem: make(chan struct{}, opts.MaxConcurrent), inflight: make(map[string]*inflightCall),
	}
}

func (e *engine) Decide(ctx context.Context, req Request) (Response, error) {
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
	key := requestKey(req)
	e.mu.Lock()
	if existing := e.inflight[key]; existing != nil {
		e.mu.Unlock()
		select {
		case <-existing.done:
			return existing.resp, nil
		case <-ctx.Done():
			return Response{Kind: req.Kind, Tier: req.Tier, Status: StatusCanceled, Error: ctx.Err().Error()}, nil
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	e.inflight[key] = call
	e.mu.Unlock()

	resp := e.run(ctx, req, runner, budget)
	e.mu.Lock()
	call.resp = resp
	delete(e.inflight, key)
	close(call.done)
	e.mu.Unlock()
	return resp, nil
}

func (e *engine) run(ctx context.Context, req Request, runner Runner, budget time.Duration) Response {
	resp := Response{Kind: req.Kind, Tier: req.Tier}
	start := time.Now()
	defer func() {
		resp.Duration = time.Since(start)
		slog.Info("fastbrain decide",
			"kind", req.Kind, "tier", req.Tier, "duration", resp.Duration,
			"status", resp.Status, "prompt_hash", promptHash(req.Prompt))
	}()

	if runner == nil {
		resp.Status, resp.Error = StatusNoRunner, "no runner configured for tier"
		return resp
	}

	if req.Kind == KindSummarizeActivity {
		// Cosmetic calls may never consume the final slot. This preserves a
		// runner for prompt recognition and control-path decisions.
		e.mu.Lock()
		if e.active >= e.opts.MaxConcurrent-1 {
			e.mu.Unlock()
			resp.Status, resp.Error = StatusDeferred, "admission reserved capacity for operational decisions"
			return resp
		}
		e.active++
		e.mu.Unlock()
		select {
		case e.sem <- struct{}{}:
		default:
			e.mu.Lock()
			e.active--
			e.mu.Unlock()
			resp.Status, resp.Error = StatusDeferred, "admission capacity unavailable"
			return resp
		}
	} else {
		select {
		case e.sem <- struct{}{}:
			e.mu.Lock()
			e.active++
			e.mu.Unlock()
		case <-ctx.Done():
			resp.Status, resp.Error = StatusCanceled, ctx.Err().Error()
			return resp
		}
	}
	defer func() {
		<-e.sem
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
	}()

	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	raw, err := runner.Run(rctx, req.Prompt)
	resp.Output.Raw = raw
	if err != nil {
		switch {
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

func requestKey(req Request) string {
	sum := sha256.Sum256([]byte(string(req.Kind) + "\x00" + string(req.Tier) + "\x00" + req.Prompt))
	return hex.EncodeToString(sum[:])
}

// promptHash is a short, non-reversible fingerprint so logs can correlate
// prompts without ever containing their text (which may hold credentials).
func promptHash(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:4])
}
