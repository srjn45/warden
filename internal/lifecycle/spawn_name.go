package lifecycle

import (
	"context"
	"strings"

	"github.com/srjn45/warden/internal/agentname"
	"github.com/srjn45/warden/internal/fastbrain"
)

// SpawnNameRunner returns the runner that derives prompt-based agent names: the
// Fast-Brain engine (Decide KindResolveAgentName, TierFast), so naming shares
// its timeout, telemetry and audit shape. Nil when no engine is wired — the
// resolver then falls back to adjective-noun codenames.
func (l *Lifecycle) SpawnNameRunner() agentname.BackendRunner {
	if l == nil || l.FastBrain == nil {
		return nil
	}
	return fastbrain.NameRunner{Engine: l.FastBrain}
}

// assignSpawnName fills req.Name when empty using role conventions, the prompt
// resolver, or an adjective-noun codename. Explicit names are left untouched.
// ExistingNames on the request drives Disambiguate for auto-generated names.
func (l *Lifecycle) assignSpawnName(ctx context.Context, req *SpawnRequest) {
	if req == nil || strings.TrimSpace(req.Name) != "" {
		return
	}
	req.Name = agentname.ResolveSpawnName(ctx, spawnNameInput(*req), l.SpawnNameRunner(), req.ExistingNames)
}

// assignJobName fills a pipeline job agent's name when unset. Pipeline stage
// naming (<pipe>:<stage>) takes priority; Role/Task are not consulted here so a
// job with role=worker does not become wkr:<tier-task>.
func (l *Lifecycle) assignJobName(ctx context.Context, req JobSpawnRequest) string {
	return agentname.ResolveSpawnName(ctx, agentname.SpawnNameInput{
		Prompt: req.Prompt,
		Pipe:   req.PipelineID,
		Stage:  req.JobID,
	}, l.SpawnNameRunner(), req.ExistingNames)
}

func spawnNameInput(req SpawnRequest) agentname.SpawnNameInput {
	return agentname.SpawnNameInput{
		Explicit: req.Name,
		Role:     req.Role,
		Prompt:   req.Prompt,
		PlanSlug: planSlugForName(req),
		// Only AutopilotTaskID (plan task id) — not Task (tier-routing registry name).
		TaskID:   req.AutopilotTaskID,
		TargetID: firstNonEmpty(req.ParentID, req.Ticket),
	}
}

// planSlugForName derives the AP:<plan> slug. Autopilot managers use
// Ticket=<scope>-autopilot (ManagerSlotID); the scope is the plan-name slug.
func planSlugForName(req SpawnRequest) string {
	ticket := strings.TrimSpace(req.Ticket)
	const suffix = "-autopilot"
	if strings.HasSuffix(ticket, suffix) {
		if scope := strings.TrimSuffix(ticket, suffix); scope != "" {
			return scope
		}
	}
	if req.PlanID != "" {
		return req.PlanID
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
