package approval

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Rule matches a parsed prompt. A present field must match; an absent field is a
// wildcard. tool/pattern are case-insensitive; regex is a Go regular expression
// (case-sensitive unless you add a (?i) flag); paths are globs against the action
// target. A rule matches when ALL of its present fields match.
//
// Foot-gun: an empty Rule{} (no fields set) matches EVERYTHING. In allow it
// approves every non-destructive recognized prompt; in deny it is a kill-switch.
type Rule struct {
	Tool    string   `yaml:"tool,omitempty" json:"tool,omitempty"`
	Pattern string   `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Regex   string   `yaml:"regex,omitempty" json:"regex,omitempty"`
	Paths   []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// Rules is the allow/deny pair under auto_approve.rules.
type Rules struct {
	Allow []Rule `yaml:"allow" json:"allow"`
	Deny  []Rule `yaml:"deny" json:"deny"`
}

// Policy is the auto-approve policy loaded from config. The top-level fields are
// the default policy; Agents holds per-agent overrides keyed by agent name or id
// (resolve the effective policy for an agent with For).
type Policy struct {
	Enabled     bool  `yaml:"enabled" json:"enabled"`
	AllowSticky bool  `yaml:"allow_sticky" json:"allow_sticky"`
	Rules       Rules `yaml:"rules" json:"rules"`
	// Agents maps an agent name (or id) to a policy override. An override fully
	// replaces the default's rules + allow_sticky for that agent and may enable
	// auto-approve for it alone; the master Enabled switch is inherited (OR'd).
	// Nested overrides ignore their own Agents map. nil ⇒ no per-agent policies.
	Agents map[string]Policy `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// Validate reports the first malformed regex in the policy (default rules and
// every per-agent override), so callers can reject a bad policy up front rather
// than silently never-matching it at evaluation time. nil ⇒ all regexes compile.
func (p Policy) Validate() error {
	check := func(rs []Rule) error {
		for _, r := range rs {
			if r.Regex == "" {
				continue
			}
			if _, err := regexp.Compile(r.Regex); err != nil {
				return fmt.Errorf("invalid regex %q: %w", r.Regex, err)
			}
		}
		return nil
	}
	if err := check(p.Rules.Allow); err != nil {
		return err
	}
	if err := check(p.Rules.Deny); err != nil {
		return err
	}
	for name, ov := range p.Agents {
		if err := check(ov.Rules.Allow); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if err := check(ov.Rules.Deny); err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
	}
	return nil
}

// HasRules reports whether any allow or deny rule is configured on this policy.
// When false the poller falls back to the legacy on/off behavior (approve every
// recognized, non-destructive prompt), keeping `auto_approve: true` working.
func (p Policy) HasRules() bool {
	return len(p.Rules.Allow) > 0 || len(p.Rules.Deny) > 0
}

// For resolves the effective policy for an agent identified by any of names
// (typically its name then its id). The first name with a matching entry in
// Agents wins: that override's rules + allow_sticky replace the default, and its
// Enabled is OR'd with the default's master switch (so an override can enable
// auto-approve for one agent even when the global default is off). With no match
// the default policy is returned. The returned policy carries no Agents map.
func (p Policy) For(names ...string) Policy {
	for _, n := range names {
		if n == "" {
			continue
		}
		if ov, ok := p.Agents[n]; ok {
			ov.Enabled = p.Enabled || ov.Enabled
			ov.Agents = nil
			return ov
		}
	}
	out := p
	out.Agents = nil
	return out
}

// Decision is the result of evaluating a prompt against the allow/deny rules only.
type Decision struct {
	Approve bool
	Reason  string // why it was skipped (for logging); "" when Approve
}

// Decide evaluates a against the allow/deny rules. It does NOT check Enabled, the
// destructive deny-list, or the affirmative index — the caller (poller) owns those.
// Empty Allow => approve nothing (fail-safe). deny wins over allow.
func (p Policy) Decide(a Approval) Decision {
	tool := toolOf(a)
	arg := argOf(a)
	paths := pathsOf(a)
	for _, r := range p.Rules.Deny {
		if r.matches(tool, arg, a.Question, paths) {
			return Decision{false, "matched a deny rule"}
		}
	}
	for _, r := range p.Rules.Allow {
		if r.matches(tool, arg, a.Question, paths) {
			return Decision{Approve: true}
		}
	}
	return Decision{false, "no allow rule matched"}
}

// matches reports whether every present field of r matches. An absent field is a
// wildcard, so an empty Rule{} matches everything (see the Rule doc foot-gun).
func (r Rule) matches(tool, arg, question string, paths []string) bool {
	if r.Tool != "" && !strings.EqualFold(r.Tool, tool) {
		return false
	}
	if r.Pattern != "" {
		if !globMatch(r.Pattern, tool+"("+arg+")") && !globMatch(r.Pattern, question) {
			return false
		}
	}
	if r.Regex != "" {
		re := compileRegex(r.Regex)
		if re == nil || (!re.MatchString(tool+"("+arg+")") && !re.MatchString(question)) {
			return false
		}
	}
	if len(r.Paths) > 0 {
		ok := false
		for _, g := range r.Paths {
			for _, cand := range paths {
				if globMatch(g, cand) {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// toolOf returns the tool name from a.Action ("Bash(rm -rf x)" -> "Bash"); "" when
// Action is not a Tool(...) header. Reuses the looksLikeAction shape.
func toolOf(a Approval) string {
	s := a.Action
	if !looksLikeAction(s) {
		return ""
	}
	open := strings.IndexByte(s, '(')
	if open <= 0 {
		return ""
	}
	return strings.TrimSpace(s[:open])
}

// argOf returns the parenthesized argument ("Bash(rm -rf x)" -> "rm -rf x"); "" if
// none.
func argOf(a Approval) string {
	s := a.Action
	if !looksLikeAction(s) {
		return ""
	}
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") || open+1 > len(s)-1 {
		return ""
	}
	return strings.TrimSpace(s[open+1 : len(s)-1])
}

// pathsOf returns candidate path tokens from the argument: the whole argument plus
// any whitespace-separated token containing '/' or '.' (covers Edit/Write/Read
// targets and path-bearing Bash args). De-duplicated; empty slice when none.
func pathsOf(a Approval) []string {
	arg := argOf(a)
	if arg == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(arg)
	for _, tok := range strings.Fields(arg) {
		if strings.ContainsAny(tok, "/.") {
			add(tok)
		}
	}
	return out
}

// regexCache memoizes compiled user regexps keyed by the raw pattern. A pattern
// that fails to compile is cached as a nil entry so a bad rule never approves and
// is not recompiled every tick.
var regexCache sync.Map // map[string]*regexp.Regexp

// compileRegex compiles (and caches) a user-supplied Go regexp. It returns nil
// for an un-compilable pattern, which matches() treats as "no match" — a
// malformed regex rule can never approve a prompt.
func compileRegex(pattern string) *regexp.Regexp {
	if v, ok := regexCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCache.Store(pattern, (*regexp.Regexp)(nil))
		return nil
	}
	regexCache.Store(pattern, re)
	return re
}

// globCache memoizes compiled glob->regexp translations keyed by the raw glob.
var globCache sync.Map // map[string]*regexp.Regexp

// globMatch reports whether s matches pattern, case-insensitively. A pattern with
// no glob metacharacter (* ? [) is matched as a substring; otherwise the glob is
// translated to an anchored regexp (** -> .*, * -> [^/]*, ? -> [^/], the rest
// escaped) and compiled once, cached by the raw glob.
func globMatch(pattern, s string) bool {
	p := strings.ToLower(pattern)
	t := strings.ToLower(s)
	if !strings.ContainsAny(p, "*?[") {
		return strings.Contains(t, p)
	}
	re := compileGlob(p)
	if re == nil {
		return false
	}
	return re.MatchString(t)
}

// compileGlob translates a (lowercased) glob into an anchored regexp, caching the
// result by the raw glob string.
func compileGlob(p string) *regexp.Regexp {
	if v, ok := globCache.Load(p); ok {
		return v.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	globCache.Store(p, re)
	return re
}

// UnmarshalYAML accepts either a scalar bool (legacy `auto_approve: true`, mapped to
// Enabled) or the nested mapping. Lets config.Load read both shapes without failing.
func (p *Policy) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var b bool
		if err := node.Decode(&b); err != nil {
			return err
		}
		p.Enabled = b
		return nil
	}
	type raw Policy // avoid recursion
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*p = Policy(r)
	return nil
}
