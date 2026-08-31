package autopilot

import (
	"path/filepath"
	"strings"
)

// canonicalPath returns the single filesystem identity used by autopilot for
// repositories, plans, enabled markers, and RunID inputs. Resolve the deepest
// existing ancestor so aliases are also folded for not-yet-created paths.
func canonicalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	cur := abs
	var suffix []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func samePath(a, b string) bool { return canonicalPath(a) == canonicalPath(b) }
