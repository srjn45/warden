package stateclassifier

import (
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/stretchr/testify/require"
)

// F-KIND-01 (docs/specs/2026-10-19-plain-terminal-inventory-and-contract.md §5):
// a plain shell session has no agent record, so even a resolver that would
// happily parse an approval from ANY screen must never produce a prompt event,
// an OnPrompt/OnUnreadable callback or a status write for it. Password,
// sudo and "[y/N]" shaped shell screens are human business.
func TestShellSessionNeverReachesPromptEngine(t *testing.T) {
	screens := map[string]string{
		"sudo":     "[sudo] password for alice: ",
		"confirm":  "Do you want to continue? [Y/n] ",
		"trust":    "Do you trust the files in this folder?\n1. Yes\n2. No",
		"numbered": "Do you want to run bash command?\n1. Yes\n2. No",
	}
	for name, screen := range screens {
		t.Run(name, func(t *testing.T) {
			mgr := newFakeManager(ptyhost.ModeOn)
			st := newFakeStore() // deliberately empty: a shell has no agent record
			stream := newFakeStream("shell-1", 1)
			stream.SetScreen(screen)
			mgr.SetTarget("shell-1", ptyhost.Target{Name: "shell-1", Stream: stream}, true)

			permissive := &mockBackend{
				id:    "mock",
				state: agentbackend.StateNeedsInput,
				approval: &agentbackend.Approval{
					Action: "Bash: rm -rf /", Question: screen,
					Options: []string{"Yes", "No"}, SelectedIdx: 1, AffirmativeIdx: 1,
				},
			}
			resolver := func(*agentstore.Agent) agentbackend.Backend { return permissive }

			cfg := DefaultConfig()
			cfg.Hysteresis.Dwell[ptyevent.StateWaitingForPermission] = 0
			c := New(cfg, mgr, st, resolver, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
			defer c.Close()

			var prompts, unreadable int
			c.OnPrompt = func(string, *ptyevent.Prompt) { prompts++ }
			c.OnUnreadable = func(string, string) { unreadable++ }

			c.getOrCreate("shell-1").snapshot.State = ptyevent.StateWorking
			c.Investigate(t.Context(), "shell-1")
			c.Investigate(t.Context(), "shell-1")

			require.Zero(t, prompts, "shell output must never reach the prompt engine")
			require.Zero(t, unreadable)
			for _, e := range stream.Emits() {
				require.NotEqual(t, ptyevent.KindPrompt, e.Kind, "no prompt event may be journaled for a shell")
			}
			require.Empty(t, st.updates, "no status write for a shell session")
		})
	}
}
