package fastbrain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Hard per-tier ceilings. Overrides may only lower them.
const (
	FastTimeout     = 1500 * time.Millisecond
	ThinkingTimeout = 10 * time.Second
)

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
}

// NewEngine returns the Engine routing TierFast/TierThinking to the given
// runners. Either may be nil; calls to a nil tier fail open.
func NewEngine(fastRunner, thinkingRunner Runner) Engine {
	return &engine{fast: fastRunner, thinking: thinkingRunner}
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
		runner, budget = e.fast, FastTimeout
	case TierThinking:
		runner, budget = e.thinking, ThinkingTimeout
	default:
		return Response{}, fmt.Errorf("%w: unknown tier %q", ErrInvalidRequest, req.Tier)
	}
	if req.Timeout > 0 && req.Timeout < budget {
		budget = req.Timeout
	}
	if ctx == nil {
		ctx = context.Background()
	}

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
		return resp, nil
	}

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
		return resp, nil
	}
	// A runner that ignored ctx may return late; honour the deadline anyway.
	if rctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			resp.Status = StatusCanceled
		} else {
			resp.Status = StatusTimeout
		}
		resp.Error = rctx.Err().Error()
		return resp, nil
	}

	obj, err := SanitizeJSON(raw)
	if err != nil {
		resp.Status, resp.Error = StatusInvalidJSON, err.Error()
		return resp, nil
	}
	resp.Output.Parsed = obj
	resp.Confidence, resp.Rationale = extractMeta(obj)
	resp.Status = StatusOK
	return resp, nil
}

// promptHash is a short, non-reversible fingerprint so logs can correlate
// prompts without ever containing their text (which may hold credentials).
func promptHash(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:4])
}
