package lifecycle

// DefaultModel is the model used when neither --model nor the model_default
// config setting is provided. Keep in sync with config's ModelDefault fallback.
var DefaultModel = "claude-sonnet-4-6"

// ResolveModel returns the model string verbatim for pass-through to
// `claude --model '<model>'`. Empty input stays empty; the caller applies
// DefaultModel / config fallback when needed. Let the claude CLI validate.
func ResolveModel(input string) string {
	return input
}

// resolveDefaultModel returns the model to use when none is explicitly provided:
// the configured default model as-is, or DefaultModel when the config leaves it empty.
func (l *Lifecycle) resolveDefaultModel() string {
	if m := l.config().GetModelDefault(); m != "" {
		return ResolveModel(m)
	}
	return DefaultModel
}

// modelOrDefault returns the model ID to use: the provided model as-is, or the
// configured default if model is empty.
func (l *Lifecycle) modelOrDefault(model string) string {
	if model != "" {
		return ResolveModel(model)
	}
	return l.resolveDefaultModel()
}
