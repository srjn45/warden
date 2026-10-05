package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/store"
)

// RouteTierMinConfidence is the confidence a Fast-Brain tier suggestion needs
// before it changes a spawn's tier.
const RouteTierMinConfidence = 0.8

// routeTierTimeout bounds the spawn-path decision; spawn never waits longer.
const routeTierTimeout = 1500 * time.Millisecond

// routeTierConfig is the optional config capability gating the tier router. It is
// a separate interface (not on ConfigProvider) so existing providers stay valid;
// one that does not implement it means "off".
type routeTierConfig interface {
	GetRouteTierUseFastBrain() bool
}

// routeTierDecision records one Fast-Brain tier suggestion for the agent's
// event log, so an operator can see why a model was chosen.
type routeTierDecision struct {
	Tier       string
	Confidence float64
	Applied    bool
}

func (d routeTierDecision) event() store.Event {
	return store.Event{
		TS:   time.Now().UTC(),
		Type: "tier-route",
		Detail: fmt.Sprintf("suggested=%s confidence=%.2f applied=%t (threshold %.2f)",
			d.Tier, d.Confidence, d.Applied, RouteTierMinConfidence),
	}
}

// routeTierByPrompt asks Fast-Brain for a complexity tier. It is the LOWEST
// precedence tier input: it runs only when the request pins no tier, task, role,
// model or ai_cli (a non-general role carries its own tier — autopilot manager,
// guardian, brain and worker spawns are therefore never touched), the setting is
// on, and a prompt exists. It returns (tier, decision): tier is non-empty only
// when the decision is confident enough to apply; decision is nil when no usable
// suggestion was obtained (off, timeout, invalid JSON, …) — every such path
// leaves resolution exactly as before.
func (l *Lifecycle) routeTierByPrompt(ctx context.Context, req SpawnRequest) (string, *routeTierDecision) {
	if l.FastBrain == nil || strings.TrimSpace(req.Prompt) == "" {
		return "", nil
	}
	if req.Tier != "" || req.Task != "" || req.Role != "" || req.Model != "" || req.Backend != "" || req.AiCli != "" {
		return "", nil
	}
	if c, ok := l.config().(routeTierConfig); !ok || !c.GetRouteTierUseFastBrain() {
		return "", nil
	}
	resp, err := l.FastBrain.Decide(ctx, fastbrain.Request{
		Kind: fastbrain.KindRouteTier, Tier: fastbrain.TierFast,
		Prompt: fastbrain.RouteTierPrompt(req.Prompt), Timeout: routeTierTimeout,
	})
	if err != nil || !resp.OK() {
		slog.Debug("spawn: tier router unavailable, keeping default resolution", "err", err, "status", resp.Status)
		return "", nil
	}
	tier, conf, ok := fastbrain.ParseRouteTier(resp)
	if !ok {
		return "", nil
	}
	d := &routeTierDecision{Tier: tier, Confidence: conf, Applied: conf >= RouteTierMinConfidence}
	if !d.Applied {
		return "", d
	}
	return tier, d
}

// applyRouteTier runs routeTierByPrompt and, when the decision is confident,
// stamps the suggested tier onto req. It returns the decision (nil when none) so
// the caller can record it on the agent's event log.
func (l *Lifecycle) applyRouteTier(ctx context.Context, req *SpawnRequest) *routeTierDecision {
	tier, d := l.routeTierByPrompt(ctx, *req)
	if tier != "" {
		req.Tier = tier
	}
	return d
}
