package agentname

import (
	"context"
	"regexp"
	"strings"
)

var slugSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// SpawnNameInput is the spawn-time context used to pick a mandatory agent name
// when the caller did not supply one explicitly.
type SpawnNameInput struct {
	// Explicit is a caller-supplied name. When non-empty after trim it is
	// returned as-is (no disambiguation); uniqueness/409 is the caller's job.
	Explicit string
	// Role is the built-in role (autopilot, worker, brain, …).
	Role string
	// Prompt drives ResolvePromptName when no role convention applies.
	Prompt string
	// PlanSlug is the plan display slug for AP:/mgr: manager names.
	PlanSlug string
	// TaskID is the autopilot/plan task id for wkr:<task> worker names.
	TaskID string
	// TargetID is the consult target (parent / subject) for brain:<target>.
	TargetID string
	// Pipe and Stage form the pipeline-stage name <pipe>:<stage>.
	Pipe  string
	Stage string
}

// ResolveSpawnName returns a store-valid agent name for a spawn.
//
// Order:
//  1. Explicit caller name (not disambiguated — collisions stay 409).
//  2. Role / pipeline convention (AP:, wkr:, brain:, <pipe>:<stage>).
//  3. Prompt-derived ResolvePromptName (fast-tier runner, 1.5s, codename fallback).
//  4. GenerateCodename for prompt-less spawns.
//
// Auto-generated names (2–4) are passed through Disambiguate so they never
// surface as 409 Conflict against existingNames.
func ResolveSpawnName(ctx context.Context, in SpawnNameInput, runner BackendRunner, existingNames map[string]bool) string {
	if name := strings.TrimSpace(in.Explicit); name != "" {
		return name
	}
	var candidate string
	if conv := conventionName(in); conv != "" {
		candidate = conv
	} else if strings.TrimSpace(in.Prompt) != "" {
		candidate, _ = ResolvePromptName(ctx, in.Prompt, runner)
	} else {
		candidate = GenerateCodename()
	}
	if existingNames == nil {
		existingNames = map[string]bool{}
	}
	return Disambiguate(candidate, existingNames)
}

// conventionName returns a role/pipeline convention name, or "" when none apply.
func conventionName(in SpawnNameInput) string {
	role := strings.ToLower(strings.TrimSpace(in.Role))
	switch role {
	case "autopilot":
		slug := sanitizeSlug(in.PlanSlug, 64)
		if slug == "" {
			slug = "unnamed"
		}
		return "AP:" + slug
	case "worker":
		task := sanitizeSlug(firstNonEmpty(in.TaskID, in.Stage), 64)
		if task == "" {
			return ""
		}
		return "wkr:" + task
	case "brain":
		target := sanitizeSlug(firstNonEmpty(in.TargetID, "consult"), 64)
		return "brain:" + target
	}
	pipe := sanitizeSlug(in.Pipe, 16)
	stage := sanitizeSlug(in.Stage, 64)
	if pipe != "" && stage != "" {
		return pipe + ":" + stage
	}
	return ""
}

// sanitizeSlug forces s into the ValidateName slug charset and truncates.
func sanitizeSlug(s string, max int) string {
	slug := slugSanitizer.ReplaceAllString(strings.TrimSpace(s), "-")
	slug = strings.Trim(slug, "-_")
	if slug == "" {
		return ""
	}
	if max > 0 && len(slug) > max {
		slug = strings.TrimRight(slug[:max], "-_")
	}
	return slug
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
