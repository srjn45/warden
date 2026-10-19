package decisionrouter

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/ptyevent"
)

type mockBackend struct {
	id       string
	state    agentbackend.State
	approval *agentbackend.Approval
}

func (m *mockBackend) ID() string                                       { return m.id }
func (m *mockBackend) DisplayName() string                              { return m.id }
func (m *mockBackend) Binary() string                                   { return m.id }
func (m *mockBackend) InstallHint() string                              { return "" }
func (m *mockBackend) LaunchCmd(agentbackend.LaunchOpts) string         { return m.id }
func (m *mockBackend) ResumeCmd(agentbackend.ResumeOpts) (string, bool) { return "", false }
func (m *mockBackend) LaunchPromptArg(string) string                    { return "" }
func (m *mockBackend) HeadlessCmd(string) ([]string, bool)              { return nil, false }
func (m *mockBackend) TranscriptPath(_, _, _ string) (string, bool)     { return "", false }
func (m *mockBackend) ParseTranscript(io.Reader) ([]agentbackend.Turn, error) {
	return nil, nil
}
func (m *mockBackend) DetectState(string) agentbackend.State { return m.state }
func (m *mockBackend) ParseApproval(string) (*agentbackend.Approval, bool) {
	if m.approval == nil {
		return nil, false
	}
	return m.approval, true
}
func (m *mockBackend) SystemPromptFlag(string) (string, bool) { return "", false }
func (m *mockBackend) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}
func (m *mockBackend) Capabilities() agentbackend.Caps { return agentbackend.Caps{} }

type mockFastBrain struct {
	response fastbrain.Response
	err      error
}

func (m *mockFastBrain) Decide(ctx context.Context, req fastbrain.Request) (fastbrain.Response, error) {
	if m.err != nil {
		return fastbrain.Response{}, m.err
	}
	return m.response, nil
}

func TestClassificationPermissionVsDecision(t *testing.T) {
	classifier := NewPromptClassifier(nil, nil)

	// 1. Tool call permission
	toolBackend := &mockBackend{
		id:    "claude",
		state: agentbackend.StateNeedsInput,
		approval: &agentbackend.Approval{
			Action:         "Bash(rm -rf node_modules)",
			Question:       "Do you want to run this command?",
			Options:        []string{"Yes", "No"},
			AffirmativeIdx: 1,
		},
	}
	res, err := classifier.Classify(context.Background(), ClassifyRequest{
		Screen:    "Bash(rm -rf node_modules)\n1. Yes\n2. No",
		Backend:   toolBackend,
		BackendID: "claude",
		Workdir:   "/repo",
	})
	if err != nil {
		t.Fatalf("classify tool permission: %v", err)
	}
	if res.Prompt == nil || res.Prompt.Class != ptyevent.ClassPermission {
		t.Fatalf("expected permission class, got %+v", res.Prompt)
	}
	if !res.Prompt.HasCap(ptyevent.CapDestructive) || !res.Prompt.HasCap(ptyevent.CapExecute) {
		t.Errorf("expected destructive and execute capabilities, got %v", res.Prompt.Capabilities)
	}

	// 2. Strategic decision (multi-choice, no tool action)
	decisionBackend := &mockBackend{
		id:    "claude",
		state: agentbackend.StateNeedsInput,
		approval: &agentbackend.Approval{
			Question:       "Which approach do you prefer?",
			Options:        []string{"Approach A: Refactor", "Approach B: Rewrite", "Type something else"},
			AffirmativeIdx: 0,
		},
	}
	res, err = classifier.Classify(context.Background(), ClassifyRequest{
		Screen:    "Which approach do you prefer?\n1. Approach A\n2. Approach B\n3. Type something else",
		Backend:   decisionBackend,
		BackendID: "claude",
		Workdir:   "/repo",
	})
	if err != nil {
		t.Fatalf("classify strategic decision: %v", err)
	}
	if res.Prompt == nil || res.Prompt.Class != ptyevent.ClassDecision {
		t.Fatalf("expected decision class, got %+v", res.Prompt)
	}
	for i, o := range res.Prompt.Options {
		if i == 2 {
			if o.Semantic != ptyevent.SemFreeform {
				t.Errorf("expected freeform semantic for option 3, got %s", o.Semantic)
			}
		} else {
			if o.Semantic != ptyevent.SemChoice {
				t.Errorf("expected choice semantic for option %d, got %s", i+1, o.Semantic)
			}
		}
	}
}

func TestCapabilityExtraction(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		target   string
		isTrust  bool
		wantCaps []ptyevent.Capability
	}{
		{
			name:     "workspace trust",
			subject:  "workspace",
			target:   "/repo",
			isTrust:  true,
			wantCaps: []ptyevent.Capability{ptyevent.CapTrust},
		},
		{
			name:     "git push publish",
			subject:  "Bash",
			target:   "git push origin main",
			wantCaps: []ptyevent.Capability{ptyevent.CapExecute, ptyevent.CapNetwork, ptyevent.CapPublish},
		},
		{
			name:     "git push force destructive",
			subject:  "Bash",
			target:   "git push --force origin main",
			wantCaps: []ptyevent.Capability{ptyevent.CapExecute, ptyevent.CapNetwork, ptyevent.CapPublish, ptyevent.CapDestructive},
		},
		{
			name:     "curl download",
			subject:  "Bash",
			target:   "curl -O https://example.com/file",
			wantCaps: []ptyevent.Capability{ptyevent.CapExecute, ptyevent.CapNetwork},
		},
		{
			name:     "read view file",
			subject:  "View",
			target:   "src/main.go",
			wantCaps: []ptyevent.Capability{ptyevent.CapRead},
		},
		{
			name:     "write edit file",
			subject:  "Edit",
			target:   "src/main.go",
			wantCaps: []ptyevent.Capability{ptyevent.CapWrite},
		},
		{
			name:     "credential access",
			subject:  "Bash",
			target:   "ssh-add ~/.ssh/id_rsa",
			wantCaps: []ptyevent.Capability{ptyevent.CapExecute, ptyevent.CapCredential},
		},
		{
			name:     "unmapped custom permission",
			subject:  "CustomTool",
			target:   "do_something_mysterious",
			wantCaps: []ptyevent.Capability{ptyevent.CapOther},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := ExtractCapabilities(c.subject, c.target, c.isTrust)
			for _, want := range c.wantCaps {
				found := false
				for _, got := range caps {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing expected capability %s in %v", want, caps)
				}
			}
		})
	}
}

func TestTargetInWorkspaceBoundsCheck(t *testing.T) {
	workdir := "/home/user/project"

	cases := []struct {
		target string
		want   bool
	}{
		{"file.txt", true},
		{"./src/app.go", true},
		{"/home/user/project/file.txt", true},
		{"/home/user/other/file.txt", false},
		{"../file.txt", false},
		{"/etc/passwd", false},
		{"/tmp/scratch.sh", false},
	}

	for _, c := range cases {
		got := IsTargetInWorkspace(c.target, workdir)
		if got != c.want {
			t.Errorf("IsTargetInWorkspace(%q, %q) = %v, want %v", c.target, workdir, got, c.want)
		}
	}
}

func TestOptionSemanticsNeverBlindlyInfersYN(t *testing.T) {
	// In strategic decision questions, "Yes (y)" or "No (n)" labels must not be treated as allow_once / deny
	sem1 := InferOptionSemantic(ptyevent.ClassDecision, 1, "Yes (recommended)", 2)
	sem2 := InferOptionSemantic(ptyevent.ClassDecision, 2, "No, choose something else", 2)
	if sem1 != ptyevent.SemChoice {
		t.Errorf("decision prompt y label: got semantic %s, want SemChoice", sem1)
	}
	if sem2 != ptyevent.SemChoice {
		t.Errorf("decision prompt n label: got semantic %s, want SemChoice", sem2)
	}

	// In permission prompts, explicit allow / deny tokens are parsed properly
	permAllow := InferOptionSemantic(ptyevent.ClassPermission, 1, "Yes, run command", 2)
	permDeny := InferOptionSemantic(ptyevent.ClassPermission, 2, "No, cancel", 2)
	permAlways := InferOptionSemantic(ptyevent.ClassPermission, 1, "Always allow for this project", 2)
	permFeedback := InferOptionSemantic(ptyevent.ClassPermission, 2, "No, and tell what to do differently", 2)

	if permAllow != ptyevent.SemAllowOnce {
		t.Errorf("permission allow: got %s, want SemAllowOnce", permAllow)
	}
	if permDeny != ptyevent.SemDeny && permDeny != ptyevent.SemCancel {
		t.Errorf("permission deny: got %s", permDeny)
	}
	if permAlways != ptyevent.SemAllowAlways {
		t.Errorf("permission always: got %s, want SemAllowAlways", permAlways)
	}
	if permFeedback != ptyevent.SemDenyFeedback {
		t.Errorf("permission feedback: got %s, want SemDenyFeedback", permFeedback)
	}
}

func TestProviderFormatAdaptationWithFastBrain(t *testing.T) {
	// Screen with unparsed menu
	pane := "Unknown CLI 3.0 Prompt:\nAllow running script?\n> 1. Yes, allow\n  2. No, refuse\n"

	recJSON, _ := json.Marshal(map[string]any{
		"is_prompt":   true,
		"question":    "Allow running script?",
		"action":      "run script.sh",
		"options":     []string{"Yes, allow", "No, refuse"},
		"affirmative": 1,
		"sticky":      false,
		"kind":        "permission",
		"confidence":  0.96,
		"rationale":   "blocking script execution prompt",
	})

	fb := &mockFastBrain{
		response: fastbrain.Response{
			Status:     fastbrain.StatusOK,
			Output:     fastbrain.Output{Raw: string(recJSON), Parsed: recJSON},
			Confidence: 0.96,
		},
	}

	store := NewMemoryLearnedShapeStore()
	classifier := NewPromptClassifier(store, fb)

	res, err := classifier.Classify(context.Background(), ClassifyRequest{
		Screen:    pane,
		BackendID: "unknowncli",
		Workdir:   "/repo",
		AgentID:   "a1",
		Now:       time.Now(),
	})
	if err != nil {
		t.Fatalf("classify format adaptation: %v", err)
	}

	if res.Prompt == nil {
		t.Fatal("expected prompt from Fast-Brain format adaptation")
	}
	if res.Prompt.Provenance != ptyevent.ProvFastBrain {
		t.Errorf("expected fastbrain provenance, got %s", res.Prompt.Provenance)
	}
	if !res.Prompt.Verified {
		t.Errorf("expected options to be verified against live pane")
	}
	if res.CandidateShape == nil {
		t.Fatal("expected candidate learned shape to be generated")
	}
	if res.CandidateShape.Status != ptyevent.ShapeCandidate {
		t.Errorf("expected candidate shape status, got %s", res.CandidateShape.Status)
	}

	// Verify store has the candidate
	sh, ok := store.Get(res.CandidateShape.ID)
	if !ok {
		t.Fatal("candidate shape was not stored")
	}
	if sh.Fingerprint != res.Prompt.Fingerprint {
		t.Errorf("fingerprint mismatch: %s vs %s", sh.Fingerprint, res.Prompt.Fingerprint)
	}
}

func TestLearnedShapeFullLifecycle(t *testing.T) {
	now := time.Now()
	store := NewMemoryLearnedShapeStore()
	pol := ptyevent.ShapePolicy{MinObservations: 3, MaxTTL: 30 * 24 * time.Hour}

	cand := ptyevent.LearnedShape{
		ID:           "shape-test",
		Fingerprint:  "fp1:lifecycle-test",
		Backend:      "mycli",
		Status:       ptyevent.ShapeCandidate,
		Class:        ptyevent.ClassPermission,
		Caps:         []ptyevent.Capability{ptyevent.CapExecute},
		Semantics:    []ptyevent.OptionSemantic{ptyevent.SemAllowOnce, ptyevent.SemDeny},
		Observations: 1,
		CreatedAt:    now,
	}

	_, err := store.AddCandidate(cand)
	if err != nil {
		t.Fatalf("add candidate: %v", err)
	}

	// Attempt to validate before reaching min observations
	_, err = store.Validate(cand.ID, now, pol)
	if err == nil {
		t.Fatal("expected validation failure with only 1 observation")
	}

	// Record 2 more observations with confirmation and fixture
	_, _ = store.RecordObservation(cand.Fingerprint, "fixture-1", true, false)
	_, _ = store.RecordObservation(cand.Fingerprint, "fixture-1", true, false)

	// Validate: candidate -> validated
	val, err := store.Validate(cand.ID, now, pol)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if val.Status != ptyevent.ShapeValidated {
		t.Fatalf("expected validated status, got %s", val.Status)
	}

	// Pin: validated -> versioned
	ver, err := store.Pin(cand.ID, now, "3.0.0", ptyevent.ShapeScopeProject, "proj-1", 7*24*time.Hour, pol)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if ver.Status != ptyevent.ShapeVersioned {
		t.Fatalf("expected versioned status, got %s", ver.Status)
	}

	// Query usability
	usable := store.FindUsable(ptyevent.ShapeQuery{
		Now:        now.Add(time.Hour),
		Backend:    "mycli",
		CLIVersion: "3.0.0",
		ProjectID:  "proj-1",
	})
	if len(usable) != 1 {
		t.Fatalf("expected 1 usable shape, got %d", len(usable))
	}

	// Usability fails for different CLI version (format drift)
	unusableDrift := store.FindUsable(ptyevent.ShapeQuery{
		Now:        now.Add(time.Hour),
		Backend:    "mycli",
		CLIVersion: "3.1.0",
		ProjectID:  "proj-1",
	})
	if len(unusableDrift) != 0 {
		t.Errorf("shape must not be usable after CLI version upgrade")
	}

	// Retire shape on contradiction or breaker trip
	ret, err := store.Retire(cand.ID, now.Add(2*time.Hour), "breaker tripped")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if ret.Status != ptyevent.ShapeRetired {
		t.Fatalf("expected retired status, got %s", ret.Status)
	}

	usableAfterRetire := store.FindUsable(ptyevent.ShapeQuery{
		Now:        now.Add(3 * time.Hour),
		Backend:    "mycli",
		CLIVersion: "3.0.0",
		ProjectID:  "proj-1",
	})
	if len(usableAfterRetire) != 0 {
		t.Errorf("retired shape must not be usable")
	}
}
