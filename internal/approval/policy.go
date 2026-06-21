package approval

import (
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Rule matches a parsed prompt. A present field must match; an absent field is a
// wildcard. tool/pattern are case-insensitive; paths are globs against the action
// target. A rule matches when ALL of its present fields match.
//
// Foot-gun: an empty Rule{} (no fields set) matches EVERYTHING. In allow it
// approves every non-destructive recognized prompt; in deny it is a kill-switch.
type Rule struct {
	Tool    string   `yaml:"tool,omitempty"`
	Pattern string   `yaml:"pattern,omitempty"`
	Paths   []string `yaml:"paths,omitempty"`
}

// Rules is the allow/deny pair under auto_approve.rules.
type Rules struct {
	Allow []Rule `yaml:"allow"`
	Deny  []Rule `yaml:"deny"`
}

// Policy is the global auto-approve policy loaded from config.
type Policy struct {
	Enabled     bool  `yaml:"enabled"`
	AllowSticky bool  `yaml:"allow_sticky"`
	Rules       Rules `yaml:"rules"`
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
