// Package llm is warden's optional local-model seam. It is opt-in and pluggable
// (an Ollama / llama.cpp HTTP endpoint); warden runs fully headless without it,
// and every caller keeps a deterministic — or headless-Claude — fallback, so a
// nil Completer or any error simply means "use the fallback". The local model
// only earns its place on fuzzy-but-cheap tasks (task classification, log /
// transcript summarization), never on deciding code changes or rewriting the
// operator's intent.
package llm

import "context"

// Completer generates a text completion for a single prompt. Implementations must
// honour ctx (deadline / cancellation): warden bounds every call with a hard
// timeout so a slow local model degrades to the fallback rather than stalling its
// caller (on CPU, local inference can be slower than just calling Claude).
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}
