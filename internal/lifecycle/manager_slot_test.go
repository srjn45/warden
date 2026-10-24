package lifecycle

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type managerSlotRunner struct {
	calls [][]string
}

func (r *managerSlotRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch args[0] {
	case "has-session":
		return "", nil
	case "show-environment":
		return "WARDEN_AUTOPILOT_RUN_ID=ap-payments\nWARDEN_PLAN_ID=plan-payments\nWARDEN_PROJECT_ID=project-payments\nWARDEN_REPO=/repo/payments\nWARDEN_ROLE=autopilot\nWARDEN_BACKEND=claude\nWARDEN_AI_CLI_SESSION_ID=claude-session\n", nil
	case "display-message":
		return "/repo/payments\n", nil
	default:
		return "", fmt.Errorf("unexpected command %v", args)
	}
}

func TestInspectManagerSlotReadsOnlyLaunchIdentity(t *testing.T) {
	runner := &managerSlotRunner{}
	l := &Lifecycle{run: runner}
	got, err := l.InspectManagerSlot(context.Background(), "payments-autopilot")
	require.NoError(t, err)
	require.Equal(t, ManagerSlotMetadata{
		TmuxSession: "payments-autopilot", RunID: "ap-payments", PlanID: "plan-payments",
		ProjectID: "project-payments", Repo: "/repo/payments", Workdir: "/repo/payments",
		Role: "autopilot", Backend: "claude", AICLISessionID: "claude-session",
	}, got)
	require.Equal(t, [][]string{
		{"tmux", "has-session", "-t", "payments-autopilot"},
		{"tmux", "show-environment", "-t", "payments-autopilot"},
		{"tmux", "display-message", "-p", "-t", "payments-autopilot", "#{pane_current_path}"},
	}, runner.calls)
}
