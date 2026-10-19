package ptyevent

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// update regenerates the golden fixtures under testdata/:
//
//	go test ./internal/ptyevent -run . -update
//
// Review the diff: a changed golden IS a contract change.
var update = flag.Bool("update", false, "rewrite golden fixtures")

// golden compares got with testdata/<name>, rewriting it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s drifted from the golden fixture (contract change? re-run with -update and review):\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// opt builds a 1-based option list.
func opts(specs ...Option) []Option {
	for i := range specs {
		specs[i].Index = i + 1
	}
	return specs
}

// mkPrompt fills fingerprint + instance key the way an adapter must.
func mkPrompt(p Prompt) *Prompt {
	if p.ShapeVersion == 0 {
		p.ShapeVersion = 1
	}
	labels := make([]string, len(p.Options))
	for i, o := range p.Options {
		labels[i] = o.Label
	}
	p.Fingerprint = Fingerprint(p.Backend, p.ShapeVersion, p.Question, labels)
	p.InstanceKey = InstanceKey(p.Fingerprint, p.Subject, p.Target)
	return &p
}

// fixturePrompts is the canonical prompt corpus. Each is written to
// testdata/prompts/<name>.json and consumed by the decision matrix.
// The Evidence.Raw excerpts are taken from the real pane fixtures under
// internal/approval/testdata and internal/agentbackend/backends/testdata.
func fixturePrompts() map[string]*Prompt {
	claudeBashRaw := "╭────────────────────────────────────────╮\n│ Bash(rm -rf node_modules)              │\n│ Do you want to proceed?                │\n│ ❯ 1. Yes                               │\n│   2. Yes, and don't ask again for rm commands in /Users/me/proj │\n│   3. No, and tell Claude what to do differently (esc) │\n╰────────────────────────────────────────╯"
	agyTrustRaw := "Accessing workspace:\n\n/work/proj\n\nDo you trust the contents of this project?\n\nAntigravity CLI requires permission to read, edit, and execute files here.\n\n> Yes, I trust this folder\n  No, exit\n\n  ↑/↓ Navigate · enter Confirm"
	claudeYesNo := func(subject, target string, caps ...Capability) Prompt {
		return Prompt{
			Backend: "claude", Class: ClassPermission, Capabilities: caps,
			Subject: subject, Target: target, Question: "Do you want to proceed?",
			Options: opts(
				Option{Label: "Yes", Key: "1", Semantic: SemAllowOnce, Scope: ScopeOnce, Default: true},
				Option{Label: "Yes, and don't ask again this session", Key: "2", Semantic: SemAllowSession, Scope: ScopeSession},
				Option{Label: "No, and tell Claude what to do differently (esc)", Key: "3", Semantic: SemDenyFeedback},
			),
			CancelPath: CancelPath{Key: "Escape"},
			Provenance: ProvParser, Confidence: 1, Verified: true,
		}
	}
	m := map[string]*Prompt{}
	m["claude_bash_rm"] = mkPrompt(Prompt{
		Backend: "claude", Class: ClassPermission, Capabilities: []Capability{CapExecute, CapDestructive},
		Subject: "Bash", Target: "rm -rf node_modules", TargetInWorkspace: true, Question: "Do you want to proceed?",
		Options: opts(
			Option{Label: "Yes", Key: "1", Semantic: SemAllowOnce, Scope: ScopeOnce, Default: true},
			Option{Label: "Yes, and don't ask again for rm commands in /Users/me/proj", Key: "2", Semantic: SemAllowAlways, Scope: ScopeProject},
			Option{Label: "No, and tell Claude what to do differently (esc)", Key: "3", Semantic: SemDenyFeedback},
		),
		CancelPath: CancelPath{Key: "Escape"},
		Provenance: ProvParser, Confidence: 1, Verified: true,
		Evidence: PromptEvidence{Raw: claudeBashRaw, FromSeq: 40, ToSeq: 44},
	})
	m["claude_read"] = mkPrompt(claudeYesNo("Read", "src/main.go", CapRead))
	m["claude_edit"] = mkPrompt(claudeYesNo("Edit", "src/main.go", CapWrite))
	m["claude_bash_test"] = mkPrompt(claudeYesNo("Bash", "go test ./...", CapExecute))
	m["claude_webfetch"] = mkPrompt(claudeYesNo("WebFetch", "https://example.com/docs", CapNetwork))
	m["claude_git_push"] = mkPrompt(claudeYesNo("Bash", "git push origin HEAD", CapExecute, CapNetwork, CapPublish))
	m["claude_keychain"] = mkPrompt(claudeYesNo("Bash", "security find-generic-password -s npm", CapExecute, CapCredential))
	m["claude_trust_own"] = mkPrompt(Prompt{
		Backend: "claude", Class: ClassPermission, Capabilities: []Capability{CapTrust},
		Subject: "workspace", Target: "/work/proj", TargetInWorkspace: true,
		Question: "Do you trust the files in this folder?",
		Options: opts(
			Option{Label: "Yes, proceed", Key: "1", Semantic: SemAllowAlways, Scope: ScopeWorkspace, Default: true},
			Option{Label: "No, exit", Key: "2", Semantic: SemCancel},
		),
		CancelPath: CancelPath{Key: "Escape", OptionIndex: 2},
		Provenance: ProvParser, Confidence: 1, Verified: true,
	})
	m["agy_trust_own"] = mkPrompt(Prompt{
		Backend: "antigravity", Class: ClassPermission, Capabilities: []Capability{CapTrust},
		Subject: "workspace", Target: "/work/proj", TargetInWorkspace: true,
		Question: "Do you trust the contents of this project?",
		Options: opts(
			Option{Label: "Yes, I trust this folder", Semantic: SemAllowAlways, Scope: ScopeWorkspace, Default: true},
			Option{Label: "No, exit", Semantic: SemCancel},
		),
		CancelPath: CancelPath{OptionIndex: 2},
		Navigate:   true,
		Provenance: ProvParser, Confidence: 1, Verified: true,
		Evidence: PromptEvidence{Raw: agyTrustRaw, FromSeq: 3, ToSeq: 5},
	})
	foreign := *m["agy_trust_own"]
	foreign.Target, foreign.TargetInWorkspace = "/home/me", false
	foreign.InstanceKey = InstanceKey(foreign.Fingerprint, foreign.Subject, foreign.Target)
	m["agy_trust_foreign"] = &foreign
	m["decision_plan_choice"] = mkPrompt(Prompt{
		Backend: "claude", Class: ClassDecision,
		Question: "Which approach should I take for the cache layer?",
		Options: opts(
			Option{Label: "Use an in-memory LRU", Key: "1", Semantic: SemChoice, Default: true},
			Option{Label: "Use Redis", Key: "2", Semantic: SemChoice},
			Option{Label: "Type something else", Key: "3", Semantic: SemFreeform},
		),
		CancelPath: CancelPath{Key: "Escape"},
		Provenance: ProvParser, Confidence: 1, Verified: true,
	})
	m["decision_plan_approval"] = mkPrompt(Prompt{
		Backend: "claude", Class: ClassDecision,
		Question: "Would you like to proceed with this plan?",
		Options: opts(
			Option{Label: "Yes, and auto-accept edits", Key: "1", Semantic: SemChoice, Default: true},
			Option{Label: "Yes, and manually approve edits", Key: "2", Semantic: SemChoice},
			Option{Label: "No, keep planning", Key: "3", Semantic: SemChoice},
		),
		CancelPath: CancelPath{Key: "Escape"},
		Provenance: ProvParser, Confidence: 1, Verified: true,
	})
	only := claudeYesNo("Edit", "src/main.go", CapWrite)
	only.Options = opts(
		Option{Label: "Yes, allow all edits during this session", Key: "1", Semantic: SemAllowSession, Scope: ScopeSession, Default: true},
		Option{Label: "No", Key: "2", Semantic: SemDeny},
	)
	m["only_standing_allow"] = mkPrompt(only)
	norefuse := claudeYesNo("Bash", "make build", CapExecute)
	norefuse.Options = opts(
		Option{Label: "Run", Key: "1", Semantic: SemAllowOnce, Default: true},
		Option{Label: "Abort turn", Key: "2", Semantic: SemCancel},
	)
	m["no_refusal_option"] = mkPrompt(norefuse)
	unv := claudeYesNo("Bash", "go test ./...", CapExecute)
	unv.Provenance, unv.Confidence, unv.Verified = ProvFastBrain, 0.99, false
	m["drift_fastbrain_unverified"] = mkPrompt(unv)
	low := claudeYesNo("Bash", "go test ./...", CapExecute)
	low.Provenance, low.Confidence = ProvFastBrain, 0.8
	m["drift_fastbrain_lowconf"] = mkPrompt(low)
	fb := claudeYesNo("Bash", "go test ./...", CapExecute)
	fb.Provenance, fb.Confidence = ProvFastBrain, 0.97
	m["fastbrain_verified"] = mkPrompt(fb)
	ls := claudeYesNo("Bash", "go test ./...", CapExecute)
	ls.Provenance, ls.Confidence, ls.LearnedShapeID = ProvLearned, 0.95, "shape-1"
	m["learned_verified"] = mkPrompt(ls)
	degraded := claudeYesNo("Bash", "go test ./...", CapExecute)
	degraded.Confidence = 0.6
	m["drift_parser_degraded"] = mkPrompt(degraded)
	unk := claudeYesNo("", "", CapOther)
	unk.Class, unk.Capabilities, unk.Question = ClassUnknown, nil, "Continue?"
	m["drift_unknown_class"] = mkPrompt(unk)
	oth := claudeYesNo("MysteryTool", "frobnicate", CapOther)
	m["unmapped_capability"] = mkPrompt(oth)
	return m
}
