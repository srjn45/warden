package stateclassifier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
)

// F-KIND-02 / F-KIND-03 on the REAL ptyhost.Manager and a real direct shell:
// the shell prints exactly what claude/codex print for a permission or trust
// prompt (as happens when a user launches one by hand inside a terminal), an
// agent RECORD with the very same name exists, and the backend would happily
// approve anything. The classifier must never see the session, so no state, no
// prompt and no status write exists for it — kind, not name or screen, gates.
func TestManuallyLaunchedAICLIInShellGetsNoAutomation(t *testing.T) {
	screen := "Do you want to proceed?\n❯ 1. Yes\n  2. No\nDo you trust the files in this folder? [y/N]"
	mgr := ptyhost.New(ptyhost.Config{
		Mode: ptyhost.ModeOn, Dir: t.TempDir(),
		Shell:   []string{"/bin/sh", "-c", "printf '%s\\n' \"$0\"; sleep 30", screen},
		Options: ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 300 * time.Millisecond, DrainGrace: 200 * time.Millisecond},
	})
	defer mgr.Close()

	const name = "collide-1"
	st := newFakeStore()
	st.agents[name] = &agentstore.Agent{ID: name, TmuxSession: name, AiCli: "mock"} // same-named AI record
	permissive := &mockBackend{id: "mock", state: agentbackend.StateNeedsInput,
		approval: &agentbackend.Approval{Action: "Bash: rm -rf /", Question: screen, Options: []string{"Yes", "No"}, AffirmativeIdx: 1}}
	cfg := DefaultConfig()
	cfg.Hysteresis.Dwell[ptyevent.StateWaitingForPermission] = 0
	c := New(cfg, mgr, st, func(*agentstore.Agent) agentbackend.Backend { return permissive }, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	defer c.Close()
	var prompts int
	c.OnPrompt = func(string, *ptyevent.Prompt) { prompts++ }

	require.NoError(t, mgr.TerminalHost().NewSession(context.Background(), name, t.TempDir()))
	d := mgr.Direct(name)
	require.NotNil(t, d)
	require.Eventually(t, func() bool { return len(d.Screen()) > 0 && strings.Contains(d.Screen(), "trust the files") }, 10*time.Second, 10*time.Millisecond)
	// Flush the dispatch pipeline deterministically: a lifecycle event after the
	// output has passed through the same ordered consumer.
	require.NoError(t, mgr.Host().KillSession(context.Background(), name))

	_, seen := c.CurrentState(name)
	require.False(t, seen, "classifier must never observe a plain shell")
	require.Zero(t, prompts)
	require.Empty(t, st.updates)
}
