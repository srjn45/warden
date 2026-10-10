package fastbrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// nameJSONSuffix asks for a JSON envelope so the reply goes through the same
// sanitize/telemetry path as every other Decide call.
const nameJSONSuffix = "\n\nReply with ONLY this JSON: {\"name\":\"<kebab-case-slug>\"}"

// NameRunner adapts an Engine to agentname.BackendRunner (structurally; this
// package does not import agentname). Each Run is one Decide(KindResolveAgentName,
// TierFast), so spawn-path naming shows up in the same telemetry/audit shape as
// every other decision. Run returns the raw slug for agentname to sanitize, and
// an error on any non-OK outcome so the caller falls back to a codename.
type NameRunner struct{ Engine Engine }

// Run asks the engine for a name. A nil engine yields an error (codename fallback).
func (n NameRunner) Run(ctx context.Context, prompt string) (string, error) {
	if n.Engine == nil {
		return "", errors.New("fastbrain: no engine")
	}
	resp, err := n.Engine.Decide(ctx, NewRequest(KindResolveAgentName, TierFast, prompt+nameJSONSuffix, ""))
	if err != nil {
		return "", err
	}
	if !resp.OK() {
		return "", fmt.Errorf("fastbrain: name decision %s: %s", resp.Status, resp.Error)
	}
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp.Output.Parsed, &v); err != nil {
		return "", err
	}
	return v.Name, nil
}
