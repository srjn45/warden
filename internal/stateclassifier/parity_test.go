package stateclassifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/terminalcontract"
)

type corpusItem struct {
	name          string
	isShell       bool
	screen        string
	backendState  agentbackend.State
	approval      *agentbackend.Approval
	expectedState ptyevent.State
	hasPrompt     bool
}

// F-PAR-02: labelled AI prompt corpus: state + permission accuracy vs legacy poller.
// Asserts StateAccuracyPct >= 98, PromptFalseApprovals = 0, and ShellClassifierEvents = 0.
func TestF_PAR_02_LabelledAIPromptCorpusAccuracyVsLegacyPoller(t *testing.T) {
	corpus := []corpusItem{
		// 1. Labelled AI Permission Prompts
		{
			name:         "claude_bash_exec",
			screen:       "Do you want to proceed?\n❯ 1. Yes\n  2. No\n",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Bash: go test ./...", Question: "Do you want to proceed?",
				Options: []string{"Yes", "No"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "claude_destructive_rm",
			screen:       "╭────────────────────────────────────────╮\n│ Bash(rm -rf node_modules)              │\n│ Do you want to proceed?                │\n│ ❯ 1. Yes                               │\n│   2. No                                │\n╰────────────────────────────────────────╯",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Bash: rm -rf node_modules", Question: "Do you want to proceed?",
				Options: []string{"Yes", "No"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "claude_file_read",
			screen:       "Claude wants to read file src/main.go\nDo you want to proceed? [y/N]",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Read: src/main.go", Question: "Do you want to proceed?",
				Options: []string{"Yes", "No"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "claude_file_edit",
			screen:       "Claude wants to edit src/main.go\n1. Allow\n2. Deny",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Edit: src/main.go", Question: "Claude wants to edit src/main.go",
				Options: []string{"Allow", "Deny"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "workspace_trust",
			screen:       "Accessing workspace:\n/work/proj\nDo you trust the contents of this project?\n> Yes, I trust this folder\n  No, exit",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Trust Workspace", Question: "Do you trust the contents of this project?",
				Options: []string{"Yes", "No"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "git_push_approval",
			screen:       "Push changes to origin/main?\n1. Yes, push\n2. No, cancel",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Bash: git push origin main", Question: "Push changes to origin/main?",
				Options: []string{"Yes", "No"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},
		{
			name:         "keychain_access",
			screen:       "Retrieve secret from keychain?\n1. Allow once\n2. Deny",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Action: "Security: keychain find-generic-password", Question: "Retrieve secret from keychain?",
				Options: []string{"Allow once", "Deny"}, AffirmativeIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForPermission,
			hasPrompt:     true,
		},

		// 2. AI Decision / Choice Prompts
		{
			name:         "menu_choice",
			screen:       "Select refactor approach:\n1. Rename symbol\n2. Extract function\n3. Inline variable",
			backendState: agentbackend.StateNeedsInput,
			approval: &agentbackend.Approval{
				Question:    "Select refactor approach",
				Options:     []string{"Rename symbol", "Extract function", "Inline variable"},
				SelectedIdx: 1,
			},
			expectedState: ptyevent.StateWaitingForDecision,
			hasPrompt:     true,
		},

		// 3. Normal AI Working Screens
		{
			name:          "compiling",
			screen:        "Compiling github.com/srjn45/warden/cmd/warden... [====================] 100%",
			backendState:  agentbackend.StateWorking,
			approval:      nil,
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
		{
			name:          "running_tests",
			screen:        "=== RUN   TestClassifier\n--- PASS: TestClassifier (0.05s)\nPASS",
			backendState:  agentbackend.StateWorking,
			approval:      nil,
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
		{
			name:          "generating_code",
			screen:        "Generating implementation files for review...",
			backendState:  agentbackend.StateWorking,
			approval:      nil,
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},

		// 4. Human Shell Screens (must NEVER be classified as AI prompts or auto-approved)
		{
			name:          "shell_sudo_password",
			isShell:       true,
			screen:        "[sudo] password for operator: ",
			backendState:  agentbackend.StateNeedsInput,
			approval:      &agentbackend.Approval{Action: "sudo", Question: "password", Options: []string{"Yes", "No"}, AffirmativeIdx: 1},
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
		{
			name:          "shell_confirm_prompt",
			isShell:       true,
			screen:        "Do you want to continue? [Y/n] ",
			backendState:  agentbackend.StateNeedsInput,
			approval:      &agentbackend.Approval{Action: "continue", Question: "continue?", Options: []string{"Y", "n"}, AffirmativeIdx: 1},
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
		{
			name:          "shell_git_commit",
			isShell:       true,
			screen:        "# Please enter the commit message for your changes.",
			backendState:  agentbackend.StateNeedsInput,
			approval:      nil,
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
		{
			name:          "shell_command_prompt",
			isShell:       true,
			screen:        "developer@workstation:~/dev/warden$ ls -la",
			backendState:  agentbackend.StateNeedsInput,
			approval:      nil,
			expectedState: ptyevent.StateWorking,
			hasPrompt:     false,
		},
	}

	correctClassifications := 0
	totalClassifications := 0
	promptFalseApprovals := 0
	shellClassifierEvents := 0

	for _, item := range corpus {
		t.Run(item.name, func(t *testing.T) {
			mgr := newFakeManager(ptyhost.ModeOn)
			st := newFakeStore()
			stream := newFakeStream(item.name, 1)
			stream.SetScreen(item.screen)
			mgr.SetTarget(item.name, ptyhost.Target{Name: item.name, Stream: stream}, true)

			if !item.isShell {
				st.agents[item.name] = &agentstore.Agent{
					ID:          item.name,
					TmuxSession: item.name,
					AiCli:       "mock",
				}
			}

			mb := &mockBackend{
				id:       "mock",
				state:    item.backendState,
				approval: item.approval,
			}
			resolver := func(ag *agentstore.Agent) agentbackend.Backend {
				if ag == nil || item.isShell {
					return nil
				}
				return mb
			}

			cfg := DefaultConfig()
			cfg.Hysteresis.Dwell[ptyevent.StateWaitingForPermission] = 0
			cfg.Hysteresis.Dwell[ptyevent.StateWaitingForDecision] = 0
			c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
			defer c.Close()

			var promptEventCount int
			c.OnPrompt = func(s string, p *ptyevent.Prompt) {
				promptEventCount++
			}

			// Pre-seed state to StateWorking
			c.getOrCreate(item.name).snapshot.State = ptyevent.StateWorking

			// Run investigation (twice to settle hysteresis)
			ctx := context.Background()
			c.Investigate(ctx, item.name)
			c.Investigate(ctx, item.name)

			snap, found := c.CurrentState(item.name)

			if item.isShell {
				// Human shell session checks: must never produce prompt events or transition state
				if promptEventCount > 0 {
					promptFalseApprovals++
					shellClassifierEvents++
				}
				for _, e := range stream.Emits() {
					if e.Kind == ptyevent.KindPrompt || e.Kind == ptyevent.KindState {
						shellClassifierEvents++
					}
				}
				require.Zero(t, promptEventCount, "human shell must never trigger OnPrompt")
				require.True(t, found)
				require.Equal(t, ptyevent.StateWorking, snap.State, "shell state must remain Working; never classify prompts")
			} else {
				// AI session checks
				totalClassifications++
				require.True(t, found, "AI session must be tracked by classifier")

				if snap.State == item.expectedState {
					correctClassifications++
				} else {
					t.Logf("[%s] got state %v, want %v", item.name, snap.State, item.expectedState)
				}

				if item.hasPrompt {
					require.GreaterOrEqual(t, promptEventCount, 1, "expected prompt event for %s", item.name)
				} else {
					require.Zero(t, promptEventCount, "expected no prompt event for %s", item.name)
				}
			}
		})
	}

	require.Greater(t, totalClassifications, 0)
	accuracyPct := float64(correctClassifications) / float64(totalClassifications) * 100.0

	// Assert numeric budgets from terminalcontract.Default
	require.GreaterOrEqual(t, accuracyPct, terminalcontract.Default.StateAccuracyPct,
		fmt.Sprintf("State accuracy %.1f%% below budget %.1f%%", accuracyPct, terminalcontract.Default.StateAccuracyPct))
	require.Equal(t, terminalcontract.Default.PromptFalseApprovals, promptFalseApprovals,
		"PromptFalseApprovals must be 0")
	require.Equal(t, terminalcontract.Default.ShellClassifierEvents, shellClassifierEvents,
		"ShellClassifierEvents must be 0")
}
