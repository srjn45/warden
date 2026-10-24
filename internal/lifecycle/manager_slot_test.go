package lifecycle

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreeFormManagerLaunchPersistsVerifiableIdentity(t *testing.T) {
	runner := &FakeRunner{}
	l := New(runner, &FakeConfig{})
	repo := t.TempDir()
	agent, err := l.Spawn(context.Background(), SpawnRequest{
		Ticket: "payments-autopilot", Cwd: repo, Repo: repo, Role: "autopilot", Backend: "codex",
		AutopilotRunID: "ap-payments", PlanID: "plan-payments", ProjectID: "project-payments",
	})
	require.NoError(t, err)
	require.NotEmpty(t, agent.Runtime.LaunchID, "launch token survives delayed backend session discovery")
	require.Empty(t, agent.AICLISessionID, "codex remains unpinned at launch")
	var launch []string
	for _, call := range runner.Calls {
		if len(call.Argv) > 2 && call.Argv[0] == "tmux" && call.Argv[1] == "new-session" {
			launch = call.Argv
			break
		}
	}
	require.Contains(t, launch, "WARDEN_REPO="+repo)
	require.Contains(t, launch, "WARDEN_AUTOPILOT_RUN_ID=ap-payments")
	require.Contains(t, launch, "WARDEN_MANAGER_LAUNCH_ID="+agent.Runtime.LaunchID)
}

type managerSlotRunner struct {
	calls   [][]string
	display string
}

func (r *managerSlotRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch args[0] {
	case "has-session":
		return "", nil
	case "show-environment":
		return "WARDEN_AUTOPILOT_RUN_ID=ap-payments\nWARDEN_PLAN_ID=plan-payments\nWARDEN_PROJECT_ID=project-payments\nWARDEN_REPO=/repo/payments\nWARDEN_ROLE=autopilot\nWARDEN_BACKEND=claude\nWARDEN_AI_CLI_SESSION_ID=claude-session\nWARDEN_MANAGER_LAUNCH_ID=launch-1\n", nil
	case "display-message":
		if r.display != "" {
			return r.display, nil
		}
		return "payments-autopilot\t/repo/payments\n", nil
	default:
		return "", fmt.Errorf("unexpected command %v", args)
	}
}

func TestInspectManagerSlotRefusesPrefixNeighbor(t *testing.T) {
	l := &Lifecycle{run: &managerSlotRunner{display: "payments-autopilot-old\t/repo/payments\n"}}
	_, err := l.InspectManagerSlot(context.Background(), "payments-autopilot")
	require.ErrorContains(t, err, "identity")
}

func TestInspectManagerSlotReadsOnlyLaunchIdentity(t *testing.T) {
	runner := &managerSlotRunner{}
	l := &Lifecycle{run: runner}
	got, err := l.InspectManagerSlot(context.Background(), "payments-autopilot")
	require.NoError(t, err)
	require.Equal(t, ManagerSlotMetadata{
		TmuxSession: "payments-autopilot", RunID: "ap-payments", PlanID: "plan-payments",
		ProjectID: "project-payments", Repo: "/repo/payments", Workdir: "/repo/payments",
		Role: "autopilot", Backend: "claude", AICLISessionID: "claude-session", LaunchID: "launch-1",
	}, got)
	require.Equal(t, [][]string{
		{"tmux", "has-session", "-t", "=payments-autopilot"},
		{"tmux", "show-environment", "-t", "=payments-autopilot"},
		{"tmux", "display-message", "-p", "-t", "=payments-autopilot", "#{session_name}\t#{pane_current_path}"},
	}, runner.calls)
}
