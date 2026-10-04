package fastbrain

import "regexp"

// Redaction is applied to every crash excerpt stored on a draft AND to every
// prompt sent to the model. Nothing here talks to the network.

var (
	reAuthHeader = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s"']+`)
	reBearer     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{6,}`)
	reSK         = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	reAIza       = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`)
	reGHPrefix   = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)
	reGHPat      = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`)

	reHome = regexp.MustCompile(`(?:/home/[^/\s:()"']+|/Users/[^/\s:()"']+|/root)(/|\b)`)
	// An absolute (or ~-rooted) path leading to a repo-relative package dir;
	// keep only the relative part, e.g. internal/planstore/store.go:142.
	reRepoPath = regexp.MustCompile(`(?m)(^|[\s(\["'])(?:~|/)(?:[^\s:()"']*/)?((?:internal|cmd)/[^\s:()"']+)`)
)

// Sanitize strips credentials and normalizes local paths. It is idempotent.
func Sanitize(s string) string {
	s = reAuthHeader.ReplaceAllString(s, "${1}[REDACTED]")
	s = reBearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = reSK.ReplaceAllString(s, "[REDACTED]")
	s = reAIza.ReplaceAllString(s, "[REDACTED]")
	s = reGHPat.ReplaceAllString(s, "[REDACTED]")
	s = reGHPrefix.ReplaceAllString(s, "[REDACTED]")
	s = reHome.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 0 && m[len(m)-1] == '/' {
			return "~/"
		}
		return "~"
	})
	s = reRepoPath.ReplaceAllString(s, "$1$2")
	return s
}
