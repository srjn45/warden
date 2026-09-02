package autopilot

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode"
)

const (
	// DefaultIntegrationBranch is the legacy global merge target. New runs treat
	// this value (and empty) as "derive autopilot/<plan>" rather than a shared
	// branch. Existing RunRecord values equal to it are grandfathered.
	DefaultIntegrationBranch = "autopilot/integration"

	// PlanBranchPlaceholder is expanded to the sanitized plan name when present
	// in autopilot.merge.target_branch (e.g. integration/{{plan}}).
	PlanBranchPlaceholder = "{{plan}}"

	derivedBranchPrefix = "autopilot/"
)

type branchResolveOpts struct {
	planName string
	runID    string
	stored   string
	template string
	taken    func(branch string) bool
}

func isLegacyIntegrationTemplate(template string) bool {
	t := strings.TrimSpace(template)
	return t == "" || t == DefaultIntegrationBranch
}

// ResolveInitIntegrationBranch applies WP9 precedence for `warden autopilot init`:
// plan-name derivation when the configured template is empty or the legacy
// global default, {{plan}} expansion, otherwise the custom global as-is.
func ResolveInitIntegrationBranch(planName, template string) (string, error) {
	return resolveIntegrationBranch(branchResolveOpts{planName: planName, template: template})
}

func resolveIntegrationBranch(opts branchResolveOpts) (string, error) {
	if stored := strings.TrimSpace(opts.stored); stored != "" {
		return stored, nil
	}
	template := strings.TrimSpace(opts.template)
	disambiguate := false
	var candidate string
	switch {
	case strings.Contains(template, PlanBranchPlaceholder):
		component, err := sanitizeBranchComponent(opts.planName)
		if err != nil {
			return "", err
		}
		candidate = strings.ReplaceAll(template, PlanBranchPlaceholder, component)
		disambiguate = true
	case isLegacyIntegrationTemplate(template):
		component, err := sanitizeBranchComponent(opts.planName)
		if err != nil {
			return "", err
		}
		candidate = derivedBranchPrefix + component
		disambiguate = true
	default:
		candidate = template
	}
	if err := validateIntegrationBranch(candidate); err != nil {
		return "", err
	}
	if disambiguate && opts.taken != nil && opts.taken(candidate) {
		dis := disambiguateBranch(candidate, opts.runID)
		if err := validateIntegrationBranch(dis); err != nil {
			return "", err
		}
		if opts.taken(dis) {
			return "", fmt.Errorf("%w: integration branch %q is already claimed", ErrRunConflict, dis)
		}
		candidate = dis
	}
	return candidate, nil
}

// sanitizeBranchComponent turns a plan name into a git-ref-safe path component
// per git check-ref-format: no trailing .lock, leading/trailing dots, "..",
// trailing slash, or all-dot names. Nested names ("/") are rejected so
// autopilot/foo cannot collide with autopilot/foo/bar.
func sanitizeBranchComponent(name string) (string, error) {
	original := strings.TrimSpace(name)
	if original == "" {
		return "", fmt.Errorf("%w: plan name yields empty integration branch component", ErrRunConflict)
	}
	if strings.ContainsAny(original, `/\`) {
		return "", fmt.Errorf("%w: plan name %q must not contain '/' (nested integration branches are not supported)", ErrRunConflict, original)
	}

	s := original
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if strings.HasSuffix(strings.ToLower(s), ".lock") {
		s = s[:len(s)-len(".lock")] + "-lock"
	}
	s = strings.Trim(s, ".")

	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r < 32 || r == 127 || r == '~' || r == '^' || r == ':' || r == '?' || r == '*' || r == '[' || r == ' ' || r == '@' || r == '{' || r == '\\':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
			prevDash = r == '-'
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" || isAllDots(out) {
		return "", fmt.Errorf("%w: plan name %q yields empty integration branch component", ErrRunConflict, original)
	}
	if strings.HasPrefix(out, ".") || strings.HasSuffix(out, ".") || strings.Contains(out, "..") {
		return "", fmt.Errorf("%w: plan name %q yields invalid integration branch component %q", ErrRunConflict, original, out)
	}
	return out, nil
}

func isAllDots(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' {
			return false
		}
	}
	return true
}

func disambiguateBranch(branch, runID string) string {
	return branch + runIDSuffix(runID)
}

func (c *Controller) branchTakenLocked(repo, selfRunID string, pending map[string]string) func(string) bool {
	return func(branch string) bool {
		if branch == "" {
			return false
		}
		if pending != nil {
			if owner, ok := pending[repo+"\x00"+branch]; ok && owner != selfRunID {
				return true
			}
		}
		for _, r := range c.runs {
			if r.repo == repo && r.runID != selfRunID && r.integrationBranch == branch {
				return true
			}
		}
		return false
	}
}

func (c *Controller) warnSameBranchLocked(repo, branch, runID string, pending map[string]string) {
	if branch == "" {
		return
	}
	others := map[string]struct{}{}
	for _, r := range c.runs {
		if r.repo == repo && r.runID != runID && r.integrationBranch == branch {
			others[r.runID] = struct{}{}
		}
	}
	if pending != nil {
		if owner, ok := pending[repo+"\x00"+branch]; ok && owner != runID {
			others[owner] = struct{}{}
		}
	}
	if len(others) == 0 {
		return
	}
	ids := make([]string, 0, len(others))
	for id := range others {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	slog.Warn("autopilot: multiple runs in the same repo resolve to the same integration branch",
		"repo", repo, "branch", branch, "run", runID, "other_runs", ids)
}

func validateIntegrationBranch(branch string) error {
	b := strings.TrimSpace(branch)
	if b == "" {
		return fmt.Errorf("%w: integration branch is empty", ErrRunConflict)
	}
	if strings.HasPrefix(b, "-") {
		return fmt.Errorf("%w: integration branch %q must not begin with '-'", ErrRunConflict, b)
	}
	if b == "@" {
		return fmt.Errorf("%w: integration branch cannot be '@'", ErrRunConflict)
	}
	if strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") {
		return fmt.Errorf("%w: integration branch %q is not a valid git ref", ErrRunConflict, b)
	}
	if strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") {
		return fmt.Errorf("%w: integration branch %q is not a valid git ref", ErrRunConflict, b)
	}
	for _, part := range strings.Split(b, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("%w: integration branch %q is not a valid git ref", ErrRunConflict, b)
		}
	}
	if strings.ContainsAny(b, " ~^:?*[\\\x7f") {
		return fmt.Errorf("%w: integration branch %q is not a valid git ref", ErrRunConflict, b)
	}
	for _, r := range b {
		if r < 32 {
			return fmt.Errorf("%w: integration branch %q is not a valid git ref", ErrRunConflict, b)
		}
	}
	return nil
}
