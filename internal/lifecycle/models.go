package lifecycle

// modelAliases maps short model names to full model IDs.
// Updated when new Claude models are released.
var modelAliases = map[string]string{
	"opus":   "claude-opus-4-8",
	"sonnet": "claude-sonnet-4-6",
	"haiku":  "claude-haiku-4-5",
	"fable":  "claude-fable-5",
}

// DefaultModel is the model used when neither --model nor the model_default
// config setting is provided. It tracks the "sonnet" alias so the implicit
// default and an explicit `--model sonnet` never resolve to different models.
var DefaultModel = modelAliases["sonnet"]

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

// resolveDefaultModel returns the model to use when none is explicitly provided:
// the configured default model (aliases expanded), or DefaultModel when the
// config leaves it empty.
func (l *Lifecycle) resolveDefaultModel() string {
	if m := l.cfg.GetModelDefault(); m != "" {
		return ResolveModel(m) // support aliases in the configured default too
	}
	return DefaultModel
}

// modelOrDefault returns the resolved model ID to use: the provided model
// (with aliases expanded), or the configured default if model is empty.
func (l *Lifecycle) modelOrDefault(model string) string {
	if model != "" {
		return ResolveModel(model)
	}
	return l.resolveDefaultModel()
}
