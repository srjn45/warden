package repl

import "strings"

// sanitizeCall scrubs hallucinated arguments out of a model-proposed tool call
// before it reaches the confirm gate or dispatch. The system prompt *asks* the
// local model not to invent a repo path, a model name, or a type it wasn't given
// (those bad args are the #1 spawn failure), but prompting is the weakest
// enforcement — a small model ignores it. This is the deterministic backstop: it
// drops the values the daemon would reject anyway, so warden falls back to its
// own sensible defaults instead of erroring out.
//
// It only ever *removes* (or canonicalises) a value; it never invents one. Three
// rules, all conservative:
//   - an empty / whitespace-only string arg is dropped (the model padded a field
//     it had nothing for);
//   - a value for one of warden's closed enums (model / permission_mode / type)
//     that isn't in range is dropped, and an in-range one is canonicalised
//     (e.g. "PR-Review" → "pr-review") so a casing slip still lands;
//   - a placeholder path the model fabricated from the prompt wording
//     ("/path/to/repo", "<repo>", "…/foo") is dropped from repo/dir fields.
func sanitizeCall(c ToolCall) ToolCall {
	if len(c.Args) == 0 {
		return c
	}
	cleaned := make(map[string]any, len(c.Args))
	for k, v := range c.Args {
		s, isStr := v.(string)
		if isStr && strings.TrimSpace(s) == "" {
			continue // an empty string is the same as omitting the field
		}
		if opts := fieldOptions(k); len(opts) > 0 && isStr {
			canon, ok := resolveOption(s, opts)
			if !ok {
				continue // out-of-range enum — omit it and let warden default
			}
			cleaned[k] = canon
			continue
		}
		if isStr && isPlaceholderPath(k, s) {
			continue // a fabricated "/path/to/..." is never a real repo
		}
		cleaned[k] = v
	}
	c.Args = cleaned
	return c
}

// isPlaceholderPath reports whether a repo/dir value is one of the template
// strings models emit when they have no real path — the system prompt calls
// "/path/to/..." out by name. Only path-bearing fields are checked, so a legit
// absolute path the operator actually gave survives untouched.
func isPlaceholderPath(key, val string) bool {
	if key != "repo" && key != "dir" {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(val))
	switch {
	case strings.HasPrefix(v, "<"): // <repo>, <path>
		return true
	case strings.Contains(v, "path/to"): // /path/to/repo, path/to/foo
		return true
	case strings.Contains(v, "..."): // …/foo, /repo/...
		return true
	case strings.Contains(v, "your-repo"), strings.Contains(v, "your_repo"):
		return true
	default:
		return false
	}
}
