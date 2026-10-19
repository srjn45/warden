package tui

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/ptyobs"
)

func TestRuntimeBodyShowsStateOwnerRouteAndDanger(t *testing.T) {
	out := runtimeBody(ptyobs.RuntimeView{
		Mode: "on", Supervised: true, State: "waiting", Reason: "permission",
		InputOwner: "warden", Route: "rule", Posture: "dangerously-execute-all",
		Danger: true, Evidence: []string{"menu"},
	})
	for _, want := range []string{"DANGER", "waiting (permission)", "warden", "rule", "menu"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if got := runtimeBody(ptyobs.RuntimeView{Mode: "off", Posture: "allowlisted"}); !strings.Contains(got, "not supervised") {
		t.Errorf("unsupervised: %q", got)
	}
}

func TestApprovalsBodyShowsDangerAndSummary(t *testing.T) {
	out := approvalsBody([]approval.View{{ID: "a1", Danger: true, Posture: "dangerously-execute-all",
		Route: "rule", Summary: &ptyobs.PermissionSummary{Text: "Run ls"}, Question: "ok?", Options: []string{"yes"}}}, 0, 80)
	for _, want := range []string{"DANGER", "Run ls", "route rule", "1. yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}
