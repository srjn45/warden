package agentbackend

import "testing"

// TerminalBackend launches the user's $SHELL (bash fallback), non-exec so the
// lifecycle's exit-capture fires when the user types `exit`.
func TestTerminalBackendLaunchCmd(t *testing.T) {
	got := TerminalBackend().LaunchCmd(LaunchOpts{
		SessionID: "sid", Name: "term-1", Model: "opus", Mode: "default",
	})
	if got != `${SHELL:-bash}` {
		t.Errorf("LaunchCmd = %q, want %q", got, `${SHELL:-bash}`)
	}
}

// The terminal is fully degraded: no resume, headless, transcript, pricing, state,
// approval parsing, or system-prompt injection.
func TestTerminalBackendDegrades(t *testing.T) {
	term := TerminalBackend()
	if _, ok := term.ResumeCmd(ResumeOpts{}); ok {
		t.Error("terminal must not support resume")
	}
	if _, ok := term.HeadlessCmd("x"); ok {
		t.Error("terminal must not support headless")
	}
	if _, ok := term.TranscriptPath("p", "w", "s"); ok {
		t.Error("terminal must not report a transcript path")
	}
	if _, ok := term.Pricing(); ok {
		t.Error("terminal must not report pricing")
	}
	if s := term.DetectState("anything"); s != StateUnknown {
		t.Errorf("terminal DetectState = %v, want Unknown", s)
	}
	if _, ok := term.ParseApproval("Do you want to proceed? 1. Yes"); ok {
		t.Error("terminal must not parse approvals")
	}
	if _, ok := term.SystemPromptFlag("guidance"); ok {
		t.Error("terminal must not inject a system prompt")
	}
}

// A terminal deliberately does NOT implement PromptSeeder — warden must never type
// the task prompt into a shell (it would execute it).
func TestTerminalBackendNotPromptSeeder(t *testing.T) {
	if _, ok := TerminalBackend().(PromptSeeder); ok {
		t.Error("terminal must not implement PromptSeeder (a shell would run the prompt)")
	}
}

// Capabilities are all off — the fully-degraded adapter.
func TestTerminalBackendCapabilities(t *testing.T) {
	c := TerminalBackend().Capabilities()
	if c.Resume || c.Headless || c.ModelSelection || c.StructuredTranscript || c.SystemPromptInject || c.SessionIDControl {
		t.Errorf("terminal Capabilities must all be false, got %+v", c)
	}
}

// The terminal adapter is NOT in the public registry (stage 6): Get("terminal")
// errors, IDs() omits it, and Detect() never surfaces it.
func TestTerminalNotRegistered(t *testing.T) {
	if _, err := Get("terminal"); err == nil {
		t.Error(`Get("terminal") must error — terminal is no longer a registered backend`)
	}
	for _, id := range IDs() {
		if id == "terminal" {
			t.Error("IDs() must not include terminal")
		}
	}
}
