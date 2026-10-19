package ptyevent

import (
	"errors"
	"testing"
	"time"
)

func TestPromptValidate(t *testing.T) {
	good := func() *Prompt {
		p := *fixturePrompts()["claude_bash_rm"]
		p.Options = append([]Option(nil), p.Options...)
		return &p
	}
	cases := []struct {
		name string
		mut  func(*Prompt)
		want error
	}{
		{"ok", func(*Prompt) {}, nil},
		{"no fingerprint", func(p *Prompt) { p.Fingerprint = "" }, ErrPromptFP},
		{"bad class", func(p *Prompt) { p.Class = "maybe" }, ErrPromptClass},
		{"bad provenance", func(p *Prompt) { p.Provenance = "vibes" }, ErrPromptProvenance},
		{"confidence > 1", func(p *Prompt) { p.Confidence = 1.5 }, ErrPromptConfidence},
		{"no options", func(p *Prompt) { p.Options = nil }, ErrPromptOptions},
		{"index gap", func(p *Prompt) { p.Options[1].Index = 5 }, ErrPromptOptions},
		{"two defaults", func(p *Prompt) { p.Options[2].Default = true }, ErrPromptOptions},
		{"bad semantic", func(p *Prompt) { p.Options[0].Semantic = "yes-please" }, ErrPromptOptions},
		{"permission without capability", func(p *Prompt) { p.Capabilities = nil }, ErrPromptCaps},
		{"bad capability", func(p *Prompt) { p.Capabilities = []Capability{"root"} }, ErrPromptCaps},
		{"cancel path out of range", func(p *Prompt) { p.CancelPath.OptionIndex = 9 }, ErrPromptCancel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good()
			c.mut(p)
			err := p.Validate()
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("Validate = %v, want %v", err, c.want)
			}
		})
	}
}

func TestConfident(t *testing.T) {
	ps := fixturePrompts()
	cases := []struct {
		name    string
		p       *Prompt
		learned bool
		want    bool
	}{
		{"parser", ps["claude_bash_test"], false, true},
		{"parser degraded below floor", ps["drift_parser_degraded"], false, false},
		{"fastbrain verified", ps["fastbrain_verified"], false, true},
		{"fastbrain unverified", ps["drift_fastbrain_unverified"], false, false},
		{"fastbrain low confidence", ps["drift_fastbrain_lowconf"], false, false},
		{"learned usable", ps["learned_verified"], true, true},
		{"learned shape not usable", ps["learned_verified"], false, false},
		{"unknown class", ps["drift_unknown_class"], false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		if got := Confident(c.p, c.learned); got != c.want {
			t.Errorf("%s: Confident = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFingerprintDrift: the fingerprint must be stable under instance
// variation (different command, path, number) and must change under SHAPE
// drift (reworded question, added/reordered/relabelled option, new shape
// version, other backend) — so a CLI release that changes its prompt
// invalidates learned shapes and pending decisions instead of being matched.
func TestFingerprintDrift(t *testing.T) {
	q := "Do you want to proceed?"
	o := []string{"Yes", "Yes, and don't ask again for rm commands in /Users/me/proj", "No, and tell Claude what to do differently (esc)"}
	base := Fingerprint("claude", 1, q, o)
	same := []struct {
		name string
		fp   string
	}{
		{"other path in option", Fingerprint("claude", 1, q, []string{o[0], "Yes, and don't ask again for rm commands in /tmp/x", o[2]})},
		{"whitespace and case", Fingerprint("claude", 1, "  do you WANT to   proceed? ", o)},
	}
	for _, s := range same {
		if s.fp != base {
			t.Errorf("%s: fingerprint changed (%s vs %s)", s.name, s.fp, base)
		}
	}
	if a, b := Fingerprint("claude", 1, "Proceed with step 3?", nil), Fingerprint("claude", 1, "Proceed with step 12?", nil); a != b {
		t.Errorf("numbers: %s vs %s", a, b)
	}
	if a, b := Fingerprint("claude", 1, `Run "make build"?`, nil), Fingerprint("claude", 1, `Run "rm -rf /"?`, nil); a != b {
		t.Errorf("quoted args: %s vs %s", a, b)
	}
	differ := []struct {
		name string
		fp   string
	}{
		{"reworded question", Fingerprint("claude", 1, "Do you want to continue?", o)},
		{"added option", Fingerprint("claude", 1, q, append(append([]string{}, o...), "Yes, allow all"))},
		{"reordered options", Fingerprint("claude", 1, q, []string{o[2], o[1], o[0]})},
		{"relabelled affirmative (injection-shaped)", Fingerprint("claude", 1, q, []string{"Yes, and approve everything forever", o[1], o[2]})},
		{"shape version", Fingerprint("claude", 2, q, o)},
		{"backend", Fingerprint("codex", 1, q, o)},
	}
	for _, d := range differ {
		if d.fp == base {
			t.Errorf("%s: fingerprint did not change", d.name)
		}
	}
	// Instance keys separate same-shape prompts about different targets.
	if InstanceKey(base, "Bash", "go test ./...") == InstanceKey(base, "Bash", "rm -rf /") {
		t.Error("instance key ignores the target")
	}
}

func TestRevalidate(t *testing.T) {
	ps := fixturePrompts()
	p := ps["claude_bash_test"]
	pd := PendingDecision{Decision: Decision{Action: ActionApprove, OptionIndex: 1, Rule: RulePermitted}, Prompt: *p, DecidedSeq: 50, DecidedAt: t0}
	live := func(mut func(*Live)) Live {
		cp := *p
		cp.Options = append([]Option(nil), p.Options...)
		l := Live{Prompt: &cp, State: StateWaitingForPermission, HumanSeq: 10, Now: t0.Add(time.Second)}
		if mut != nil {
			mut(&l)
		}
		return l
	}
	other := ps["claude_git_push"] // same shape, different target
	cases := []struct {
		name string
		pd   PendingDecision
		live Live
		want string
	}{
		{"unchanged", pd, live(nil), RevalOK},
		{"prompt gone", pd, live(func(l *Live) { l.Prompt = nil }), RevalPromptGone},
		{"state moved on", pd, live(func(l *Live) { l.State = StateWorking }), RevalStateChanged},
		{"different shape", pd, live(func(l *Live) { l.Prompt = ps["claude_bash_rm"] }), RevalPromptChanged},
		{"same box, different command (TOCTOU swap)", pd, live(func(l *Live) { l.Prompt = other }), RevalTargetChanged},
		{"option relabelled", pd, live(func(l *Live) { l.Prompt.Options[0].Label = "Yes, always" }), RevalOptionMoved},
		{"option semantic changed", pd, live(func(l *Live) { l.Prompt.Options[0].Semantic = SemAllowAlways }), RevalOptionMoved},
		{"option vanished", pd, live(func(l *Live) { l.Prompt.Options = l.Prompt.Options[:0] }), RevalOptionMoved},
		{"human typed since", pd, live(func(l *Live) { l.HumanSeq = 51 }), RevalHumanIntervened},
		{"stale", pd, live(func(l *Live) { l.Now = t0.Add(time.Minute) }), RevalStale},
		{"ask_human writes nothing", PendingDecision{Decision: Decision{Action: ActionAskHuman}, Prompt: *p}, live(nil), RevalNotActionable},
	}
	if other.Fingerprint != p.Fingerprint {
		t.Fatal("fixture precondition: git push and go test prompts share a shape")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Revalidate(c.pd, c.live, 0)
			got := RevalOK
			if err != nil {
				var re *RevalidationError
				if !errors.As(err, &re) {
					t.Fatalf("error %v is not a RevalidationError", err)
				}
				got = re.Code
			}
			if got != c.want {
				t.Fatalf("Revalidate = %s (%v), want %s", got, err, c.want)
			}
		})
	}
}

func TestLearnedShapeLifecycle(t *testing.T) {
	pol := DefaultShapePolicy
	cand := LearnedShape{ID: "shape-1", Fingerprint: "fp1:x", Backend: "claude", Status: ShapeCandidate,
		Class: ClassPermission, Caps: []Capability{CapExecute}, Semantics: []OptionSemantic{SemAllowOnce, SemDeny}, CreatedAt: t0}
	q := ShapeQuery{Now: t0.Add(time.Hour), Backend: "claude", CLIVersion: "2.1.0", ProjectID: "proj-a", SessionID: "s-1"}

	if cand.Usable(q) {
		t.Fatal("candidate must never be usable")
	}
	if _, err := cand.Validate(t0, pol); !errors.Is(err, ErrShapeEvidence) {
		t.Fatalf("validate with no evidence: %v", err)
	}
	cand.Observations = 3
	if _, err := cand.Validate(t0, pol); !errors.Is(err, ErrShapeEvidence) {
		t.Fatalf("self-confirmed shape must not validate: %v", err)
	}
	cand.HumanConfirmed = true
	if _, err := cand.Validate(t0, pol); !errors.Is(err, ErrShapeEvidence) {
		t.Fatalf("fixture-less shape must not validate: %v", err)
	}
	cand.Fixtures = []string{"claude/bash-go-test.json"}
	val, err := cand.Validate(t0, pol)
	if err != nil || val.Status != ShapeValidated {
		t.Fatalf("validate: %v %+v", err, val)
	}
	if val.Usable(q) {
		t.Fatal("validated but unversioned shape must not be usable")
	}
	if _, err := cand.Pin(t0, "2.1.0", ShapeScopeProject, "proj-a", 0, pol); !errors.Is(err, ErrShapeTransition) {
		t.Fatalf("candidate → versioned skip: %v", err)
	}
	if _, err := val.Pin(t0, "", ShapeScopeProject, "proj-a", 0, pol); !errors.Is(err, ErrShapeEvidence) {
		t.Fatalf("unpinned CLI version: %v", err)
	}
	if _, err := val.Pin(t0, "2.1.0", ShapeScopeProject, "", 0, pol); !errors.Is(err, ErrShapeEvidence) {
		t.Fatalf("unscoped: %v", err)
	}
	ver, err := val.Pin(t0, "2.1.0", ShapeScopeProject, "proj-a", 365*24*time.Hour, pol)
	if err != nil || ver.Status != ShapeVersioned || ver.Version != 1 || !ver.ExpiresAt.Equal(t0.Add(pol.MaxTTL)) {
		t.Fatalf("pin: %v %+v (ttl must be capped)", err, ver)
	}
	usable := []struct {
		name string
		q    ShapeQuery
		want bool
	}{
		{"in scope", q, true},
		{"other project", ShapeQuery{Now: q.Now, Backend: "claude", CLIVersion: "2.1.0", ProjectID: "proj-b"}, false},
		{"CLI upgraded (format drift risk)", ShapeQuery{Now: q.Now, Backend: "claude", CLIVersion: "2.2.0", ProjectID: "proj-a"}, false},
		{"other backend", ShapeQuery{Now: q.Now, Backend: "codex", CLIVersion: "2.1.0", ProjectID: "proj-a"}, false},
		{"expired", ShapeQuery{Now: t0.Add(31 * 24 * time.Hour), Backend: "claude", CLIVersion: "2.1.0", ProjectID: "proj-a"}, false},
	}
	for _, u := range usable {
		if got := ver.Usable(u.q); got != u.want {
			t.Errorf("Usable(%s) = %v, want %v", u.name, got, u.want)
		}
	}
	if _, err := ver.Pin(t0.Add(time.Hour), "2.1.0", ShapeScopeProject, "proj-a", 0, pol); !errors.Is(err, ErrShapeTransition) {
		t.Fatalf("renewing an unexpired shape: %v", err)
	}
	renewed, err := ver.Pin(t0.Add(31*24*time.Hour), "2.1.0", ShapeScopeProject, "proj-a", 0, pol)
	if err != nil || renewed.Version != 2 {
		t.Fatalf("renew: %v %+v", err, renewed)
	}
	ret := renewed.Retire(t0.Add(32*24*time.Hour), "breaker tripped")
	if ret.Usable(ShapeQuery{Now: t0.Add(32 * 24 * time.Hour), Backend: "claude", CLIVersion: "2.1.0", ProjectID: "proj-a"}) {
		t.Fatal("retired shape usable")
	}
	if _, err := ret.Validate(t0, pol); !errors.Is(err, ErrShapeTransition) {
		t.Fatal("retired shape revived")
	}
	// User-scope shapes need no scope id; session scope must match exactly.
	user, _ := val.Pin(t0, "2.1.0", ShapeScopeUser, "", 0, pol)
	if !user.Usable(ShapeQuery{Now: q.Now, Backend: "claude", CLIVersion: "2.1.0", ProjectID: "anything"}) {
		t.Fatal("user-scope shape not usable across projects")
	}
	sess, _ := val.Pin(t0, "2.1.0", ShapeScopeSession, "s-1", 0, pol)
	if sess.Usable(ShapeQuery{Now: q.Now, Backend: "claude", CLIVersion: "2.1.0", SessionID: "s-2"}) {
		t.Fatal("session-scope shape leaked to another session")
	}
}
