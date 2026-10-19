package decisionrouter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/ptyevent"
)

const agyFixtures = "../agentbackend/backends/testdata/antigravity"

func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(agyFixtures, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

func normalizeAgy(t *testing.T, pane, workdir string) *ptyevent.Prompt {
	t.Helper()
	b := backends.Antigravity{}
	app, ok := b.ParseApproval(pane)
	if !ok {
		t.Fatalf("agy parser found no approval")
	}
	p, err := NormalizeApproval(b, app, NormalizeContext{BackendID: "antigravity", CLIVersion: "1.2.17", Screen: pane, Workdir: workdir})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return p
}

func semantics(p *ptyevent.Prompt) []ptyevent.OptionSemantic {
	var out []ptyevent.OptionSemantic
	for _, o := range p.Options {
		out = append(out, o.Semantic)
	}
	return out
}

func eqSems(a, b []ptyevent.OptionSemantic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Each captured Antigravity prompt normalizes into the contract with the
// adapter's exact semantics. "No, cancel" is a refusal (not the contract's
// cancel) and "No, exit" on trust is the cancel that auto-deny never picks.
func TestNormalizeAntigravityFixtures(t *testing.T) {
	cases := []struct {
		file    string
		subject string
		target  string
		caps    []ptyevent.Capability
		sems    []ptyevent.OptionSemantic
		cancel  ptyevent.CancelPath
		def     int
	}{
		{
			file: "approval.txt", subject: "Bash", target: "echo hello-from-agy",
			caps:   []ptyevent.Capability{ptyevent.CapExecute},
			sems:   []ptyevent.OptionSemantic{ptyevent.SemAllowOnce, ptyevent.SemAllowSession, ptyevent.SemAllowAlways, ptyevent.SemDeny},
			cancel: ptyevent.CancelPath{Key: "Escape"}, def: 1,
		},
		{
			file: "approval-run-command.txt", subject: "Bash", target: "git rev-parse HEAD origin/main",
			caps:   []ptyevent.Capability{ptyevent.CapExecute},
			sems:   []ptyevent.OptionSemantic{ptyevent.SemAllowOnce, ptyevent.SemAllowSession, ptyevent.SemAllowAlways, ptyevent.SemDeny},
			cancel: ptyevent.CancelPath{Key: "Escape"}, def: 1,
		},
		{
			file: "approval-file-access.txt", subject: "Read",
			target: "/home/srjn45/dev/warden/.worktrees/worker-b730ae8a/internal/daemon/oapi/api.gen.go",
			caps:   []ptyevent.Capability{ptyevent.CapRead},
			sems:   []ptyevent.OptionSemantic{ptyevent.SemAllowOnce, ptyevent.SemDeny},
			cancel: ptyevent.CancelPath{Key: "Escape"}, def: 1,
		},
		{
			file: "trust-prompt.txt", subject: "workspace",
			target: "/tmp/claude-1000/-home-srjn45-dev-warden--worktrees-development-ec875845/9d3d7661-b366-43d4-84c0-ed70771b32b9/scratchpad/agytrust.dHzcZl",
			caps:   []ptyevent.Capability{ptyevent.CapTrust},
			sems:   []ptyevent.OptionSemantic{ptyevent.SemAllowAlways, ptyevent.SemCancel},
			cancel: ptyevent.CancelPath{Key: "Escape", OptionIndex: 2}, def: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			p := normalizeAgy(t, agyFixture(t, tc.file), "/nonexistent-workdir")
			if p.Class != ptyevent.ClassPermission || p.Provenance != ptyevent.ProvParser || !p.Verified {
				t.Fatalf("class/provenance/verified: %s %s %v", p.Class, p.Provenance, p.Verified)
			}
			if p.Subject != tc.subject || p.Target != tc.target {
				t.Errorf("subject/target = %q/%q, want %q/%q", p.Subject, p.Target, tc.subject, tc.target)
			}
			for _, c := range tc.caps {
				if !p.HasCap(c) {
					t.Errorf("missing capability %s in %v", c, p.Capabilities)
				}
			}
			if !eqSems(semantics(p), tc.sems) {
				t.Errorf("semantics = %v, want %v", semantics(p), tc.sems)
			}
			if p.CancelPath != tc.cancel {
				t.Errorf("cancel = %+v, want %+v", p.CancelPath, tc.cancel)
			}
			if got := defaultOptionIndex(p.Options); got != tc.def {
				t.Errorf("default option = %d, want %d", got, tc.def)
			}
			if !p.Navigate {
				t.Errorf("agy menus are cursor-navigated")
			}
			if !ptyevent.Confident(p, false) {
				t.Errorf("adapter-shaped prompt must be confident")
			}
		})
	}
}

// A trust target outside the session worktree is not "own worktree".
func TestNormalizeTrustWorkspaceScope(t *testing.T) {
	pane := agyFixture(t, "trust-prompt.txt")
	scratch := "/tmp/claude-1000/-home-srjn45-dev-warden--worktrees-development-ec875845/9d3d7661-b366-43d4-84c0-ed70771b32b9/scratchpad/agytrust.dHzcZl"
	if p := normalizeAgy(t, pane, scratch); !p.TargetInWorkspace {
		t.Errorf("trust of the session's own worktree must be in-workspace")
	}
	if p := normalizeAgy(t, pane, "/home/someone/else"); p.TargetInWorkspace {
		t.Errorf("trust of a foreign directory must not be in-workspace")
	}
	// No worktree known: a trust prompt is never assumed to be inside it.
	if p := normalizeAgy(t, pane, ""); p.TargetInWorkspace {
		t.Errorf("unknown workdir must not imply own-worktree trust")
	}
}

// Target-derived capabilities may be added to the adapter's, never removed.
func TestNormalizeUnionsTargetCapabilities(t *testing.T) {
	pane := strings.Replace(agyFixture(t, "approval-run-command.txt"), "git rev-parse HEAD origin/main", "git push origin main", -1)
	p := normalizeAgy(t, pane, "/w")
	for _, c := range []ptyevent.Capability{ptyevent.CapExecute, ptyevent.CapPublish, ptyevent.CapNetwork} {
		if !p.HasCap(c) {
			t.Errorf("git push should carry %s, got %v", c, p.Capabilities)
		}
	}
}

// A reworded agy menu falls outside the adapter's vocabulary: the shaper
// declines, generic inference must NOT invent a confident reading from the
// option positions.
func TestNormalizeDriftedMenuIsNotConfident(t *testing.T) {
	pane := agyFixture(t, "approval-run-command.txt")
	pane = strings.NewReplacer(
		"Yes, run command", "Go ahead",
		"No, cancel", "Stop",
		"Yes, and always allow in this conversation for commands that start with 'git rev-parse'", "Sure, remember",
		"Yes, and always allow for commands that start with 'git rev-parse' (Persist to settings.json)", "Sure, forever",
	).Replace(pane)
	b := backends.Antigravity{}
	app, ok := b.ParseApproval(pane)
	if !ok {
		t.Fatalf("parser should still find the menu")
	}
	if _, shaped := b.ShapePrompt(app); shaped {
		t.Fatalf("shaper must decline wording it has never seen")
	}
	p, err := NormalizeApproval(b, app, NormalizeContext{BackendID: "antigravity", Screen: pane, Workdir: "/w"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ptyevent.Confident(p, false) {
		t.Fatalf("a menu whose meaning was guessed from position must not be confident: %+v", p.Options)
	}
	dec := ptyevent.Decide(ptyevent.DecideInput{Posture: ptyevent.PostureDangerouslyExecuteAll, Prompt: p})
	if dec.Action != ptyevent.ActionNeedsAttention {
		t.Fatalf("drifted menu in dangerous posture = %s, want needs_attention", dec.Action)
	}
}

// A menu that names no action is a question, whatever its Yes option says.
func TestNormalizeQuestionWithYesIsDecision(t *testing.T) {
	app := &agentbackend.Approval{
		Question:    "Would you like to proceed with this plan?",
		Options:     []string{"Yes, and auto-accept edits", "Yes, and manually approve edits", "No, keep planning"},
		SelectedIdx: 1, AffirmativeIdx: 1,
	}
	p, err := NormalizeApproval(nil, app, NormalizeContext{BackendID: "claude", Screen: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != ptyevent.ClassDecision {
		t.Fatalf("class = %s, want decision", p.Class)
	}
	if d := ptyevent.Decide(ptyevent.DecideInput{Posture: ptyevent.PostureDangerouslyExecuteAll, Prompt: p}); d.Action != ptyevent.ActionRouteDecision {
		t.Fatalf("dangerous posture must route a question, got %s", d.Action)
	}
}

// Secrets that scrolled into the pane never reach the Prompt.
func TestNormalizeRedactsEvidence(t *testing.T) {
	secret := "sk-" + "ant-" + "api03-" + strings.Repeat("A1b2", 8)
	pane := strings.Replace(agyFixture(t, "approval-run-command.txt"), "git rev-parse HEAD origin/main", "curl -H 'x-api-key: "+secret+"' https://example.com", -1)
	p := normalizeAgy(t, pane, "/w")
	for name, s := range map[string]string{"target": p.Target, "evidence": p.Evidence.Raw, "question": p.Question} {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaks the secret: %q", name, s)
		}
	}
	if len(p.Evidence.Redactions) == 0 {
		t.Errorf("redactions must be recorded")
	}
}
