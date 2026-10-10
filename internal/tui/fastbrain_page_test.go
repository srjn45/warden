package tui

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/fastbrain"
)

func fbFake() *fakeAPI {
	return &fakeAPI{
		fbTel: fastbrain.TelemetrySnapshot{
			MaxConcurrent: 3,
			Controls: []fastbrain.KindControl{
				{Kind: fastbrain.KindSummarizeActivity},
				{Kind: fastbrain.KindResolveAgentName, Paused: true, Source: "operator"},
				{Kind: fastbrain.KindPRSummary, Paused: true, Source: "config"},
			},
		},
		fbDecisions: []fastbrain.Decision{{Kind: fastbrain.KindResolveAgentName, Outcome: "ok", FinalAction: "decided", Provider: "claude"}},
	}
}

func TestFastBrainPageOpenRenderAndControl(t *testing.T) {
	f := fbFake()
	m := newListPane(f, "", "")
	m.ready = true
	updated, cmd := m.handleKey(key("F"))
	m = updated.(controlPaneModel)
	if m.mode != modeFastBrain || cmd == nil {
		t.Fatalf("F should open modeFastBrain with a load cmd, mode=%v", m.mode)
	}
	nm, _ := m.Update(cmd())
	m = nm.(controlPaneModel)
	body := fastBrainBody(m.fbTel, m.fbDecisions, m.fbCursor)
	for _, want := range []string{"summarize_activity", "paused", "claude", "decided"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// p on the first (running) kind pauses it.
	_, cmd = m.handleKey(key("p"))
	if cmd == nil {
		t.Fatal("p should issue a control cmd")
	}
	cmd()
	if len(f.fbControlCalls) != 1 || f.fbControlCalls[0] != "summarize_activity=true" {
		t.Fatalf("calls=%v", f.fbControlCalls)
	}
	// Config-disabled kinds cannot be resumed from the TUI.
	m.fbCursor = 2
	um, cmd := m.handleKey(key("p"))
	if cmd != nil || !strings.Contains(um.(controlPaneModel).status, "disabled in config") {
		t.Fatalf("config kind should refuse resume, status=%q", um.(controlPaneModel).status)
	}
	// esc returns to normal.
	um, _ = m.handleKey(key("esc"))
	if um.(controlPaneModel).mode != modeNormal {
		t.Fatal("esc should leave the page")
	}
}
