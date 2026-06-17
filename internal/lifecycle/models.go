package lifecycle

import "os"

// modelAliases maps short model names to full model IDs.
// Updated when new Claude models are released.
var modelAliases = map[string]string{
	"opus":   "claude-opus-4-8",
	"sonnet": "claude-sonnet-4-6",
	"haiku":  "claude-haiku-4-5",
	"fable":  "claude-fable-5",
}

// ResolveModel maps short alias to full model ID, or returns input unchanged
// if it's already a full ID or unknown. Let claude CLI validate unknown models.
func ResolveModel(input string) string {
	if input == "" {
		return ""
	}
	if full, ok := modelAliases[input]; ok {
		return full
	}
	return input // assume it's already a full model ID
}

// resolveDefaultModel returns the model to use when none is explicitly provided.
// Checks WARDEN_MODEL_DEFAULT env var, falls back to claude-sonnet-4-5.
func resolveDefaultModel() string {
	if envModel := os.Getenv("WARDEN_MODEL_DEFAULT"); envModel != "" {
		return ResolveModel(envModel) // support aliases in env var too
	}
	return "claude-sonnet-4-5"
}

// modelOrDefault returns the resolved model ID to use: the provided model
// (with aliases expanded), or the default if model is empty.
func modelOrDefault(model string) string {
	if model != "" {
		return ResolveModel(model)
	}
	return resolveDefaultModel()
}
