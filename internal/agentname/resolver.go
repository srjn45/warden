package agentname

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// resolveTimeout is the hard ceiling for a subscription AI CLI name lookup.
// Naming must never stall spawn; past this the adjective-noun fallback wins.
const resolveTimeout = 1500 * time.Millisecond

// maxResolvedNameLen is the slug length cap for prompt-derived names (stricter
// than store.ValidateName's 32 so list columns stay readable).
const maxResolvedNameLen = 20

// nameResolveInstruction is the one-shot prompt injected before the user task.
// The model must reply with ONLY a kebab-case slug — no prose, ticks, or punctuation.
const nameResolveInstruction = "Given this user task prompt, output ONLY a concise kebab-case slug of 2 to 4 words (maximum 20 characters, lowercase letters and hyphens only). Do not include any explanations, markdown ticks, or punctuation.\n\nTask: "

// BackendRunner invokes the active subscription AI CLI on a fast-tier model
// (Haiku / Flash / GPT-4o-mini and peers) for a single headless completion.
// Implementations must honour ctx cancellation so the 1.5s naming deadline can
// abort a slow CLI. The agentname package stays store- and backend-free; the
// daemon wires a concrete runner at spawn time.
type BackendRunner interface {
	Run(ctx context.Context, prompt string) (string, error)
}

// RunnerFunc adapts a plain function to BackendRunner.
type RunnerFunc func(ctx context.Context, prompt string) (string, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// ResolvePromptName derives a short kebab-case agent name from prompt via the
// fast-tier subscription runner. It always returns a store-valid name: on
// timeout, CLI/network error, empty/invalid model output, or a nil runner it
// falls back to GenerateCodename(). The error is non-nil only when the AI path
// failed and the caller may want to log the reason; the returned name is still
// usable either way.
func ResolvePromptName(ctx context.Context, prompt string, runner BackendRunner) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || runner == nil {
		return GenerateCodename(), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	out, err := runner.Run(runCtx, nameResolveArg(prompt))
	if err != nil {
		return GenerateCodename(), err
	}
	if name := sanitizeResolvedName(out); name != "" {
		return name, nil
	}
	return GenerateCodename(), nil
}

// nameResolveArg builds the one-shot naming prompt, capping the user task so a
// huge description cannot blow up the fast-tier request.
func nameResolveArg(prompt string) string {
	const max = 2000
	if len(prompt) > max {
		prompt = prompt[:max]
	}
	return nameResolveInstruction + prompt
}

// sanitizeResolvedName normalizes a free-form model reply into a 2–4 word
// kebab-case slug of at most 20 lowercase letters and hyphens. Returns "" when
// nothing usable remains (caller falls back to GenerateCodename).
func sanitizeResolvedName(out string) string {
	line := firstContentLine(out)
	line = stripConversationalPrefix(line)
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "`\"'")
	line = strings.ToLower(line)

	var b strings.Builder
	prevHyphen := false
	for _, r := range line {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevHyphen = false
		case r == '-' || r == '_' || r == ' ' || r == '\t':
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		default:
			// Digits, punctuation, and other runes are dropped (prompt contract
			// is letters + hyphens only).
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return ""
	}
	words := strings.Split(name, "-")
	if len(words) < 2 || len(words) > 4 {
		return ""
	}
	for _, w := range words {
		if w == "" || !isAlphaWord(w) {
			return ""
		}
	}
	if len(name) > maxResolvedNameLen {
		// Truncate on a hyphen boundary so we never keep a partial word.
		name = name[:maxResolvedNameLen]
		if i := strings.LastIndexByte(name, '-'); i > 0 {
			name = name[:i]
		} else {
			return ""
		}
		words = strings.Split(name, "-")
		if len(words) < 2 {
			return ""
		}
	}
	return name
}

// firstContentLine returns the first non-empty line, skipping markdown fence
// markers (``` / ```text) so a fenced reply still yields the slug.
func firstContentLine(out string) string {
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") {
			continue
		}
		return line
	}
	return ""
}

// stripConversationalPrefix removes common chatty lead-ins models prepend
// despite the "ONLY a slug" instruction (e.g. "Sure, here is: `foo-bar`").
func stripConversationalPrefix(s string) string {
	s = strings.TrimSpace(s)
	for {
		lower := strings.ToLower(s)
		trimmed := false
		for _, p := range []string{
			"sure,", "sure!", "sure ",
			"here's", "here is", "here you go",
			"the name is", "the name",
			"the slug is", "the slug",
			"the handle is", "the handle",
			"output:", "result:", "name:", "slug:",
		} {
			if strings.HasPrefix(lower, p) {
				s = strings.TrimSpace(s[len(p):])
				s = strings.TrimLeft(s, ":,- ")
				trimmed = true
				break
			}
		}
		if !trimmed {
			return s
		}
	}
}

func isAlphaWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}
