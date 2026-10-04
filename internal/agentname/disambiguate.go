package agentname

import (
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// maxNameLen mirrors the plain-name limit enforced by store.ValidateName.
const maxNameLen = 32

// Disambiguate returns candidate if it is valid and unused, otherwise the
// candidate with the first free numeric suffix (-2, -3, ...). The base is
// truncated as needed so the result stays within store.ValidateName limits.
// An empty or invalid candidate is replaced by GenerateCodename().
func Disambiguate(candidate string, existingNames map[string]bool) string {
	if store.ValidateName(candidate) != nil {
		candidate = GenerateCodename()
	}
	if !existingNames[candidate] {
		return candidate
	}
	// Prefixed names (e.g. "wkr:x") may exceed the plain limit; truncate the
	// base so the whole name stays valid under whichever pattern it matched.
	limit := maxNameLen
	if strings.Contains(candidate, ":") {
		limit = len(candidate[:strings.Index(candidate, ":")+1]) + 64
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		base := candidate
		if len(base)+len(suffix) > limit {
			base = base[:limit-len(suffix)]
		}
		if name := base + suffix; !existingNames[name] {
			return name
		}
	}
}
