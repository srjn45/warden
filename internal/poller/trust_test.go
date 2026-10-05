package poller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/stretchr/testify/require"
)

// trustDeps is stubDeps with a live-ish Claude trust dialog: a Down keystroke
// moves the ❯ cursor to the second option (unless dropKeys swallows it) and
// Enter dismisses the dialog.
type trustDeps struct {
	*stubDeps
	dropKeys bool
}

func (d *trustDeps) SendKeys(ctx context.Context, tmuxSession, key string) error {
	_ = d.stubDeps.SendKeys(ctx, tmuxSession, key)
	if d.dropKeys {
		return nil
	}
	switch key {
	case "Down":
		p := d.panes[tmuxSession]
		p = strings.Replace(p, " ❯ No, exit", "   No, exit", 1)
		p = strings.Replace(p, "   Yes, I trust this folder", " ❯ Yes, I trust this folder", 1)
		d.panes[tmuxSession] = p
	case "Enter":
		d.panes[tmuxSession] = "❯ \n"
	}
	return nil
}

func claudeTrustPane(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agentbackend", "backends", "testdata", "claude", "trust-prompt.txt"))
	require.NoError(t, err)
	return string(b)
}

func newTrustPoller(t *testing.T, d Deps) *Poller {
	t.Helper()
	old := answerVerifyDelay
	answerVerifyDelay = 0
	t.Cleanup(func() { answerVerifyDelay = old })
	p := New(d, 30*time.Second)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return backends.Claude{} }
	return p
}

// TestTrustPromptAnsweredWithoutAutoApprove is the headline behavior: with
// trust_workspace on, the launch-time trust dialog is answered "yes" even though
// the auto-approve policy is off, by moving the cursor off "No, exit" first.
func TestTrustPromptAnsweredWithoutAutoApprove(t *testing.T) {
	d := &trustDeps{stubDeps: &stubDeps{panes: map[string]string{"tmux-1": claudeTrustPane(t)}}}
	p := newTrustPoller(t, d)
	p.SetTrustWorkspace(true)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}

	p.tryTrustPrompt(context.Background(), s, d.panes["tmux-1"])

	require.Equal(t, []string{"Down", "Enter"}, d.sentSequence("tmux-1"))
	evs := d.recordedEvents("agent-1")
	require.Len(t, evs, 1)
	require.Equal(t, "workspace_trusted", evs[0].Type)
	require.Contains(t, evs[0].Detail, "/home/dev/project")
}

// TestTrustPromptNeverConfirmsNoExit: when the cursor move does not register,
// Enter (which would pick the highlighted "No, exit" and quit the CLI) is never
// sent, and the attempt cap stops the retries.
func TestTrustPromptNeverConfirmsNoExit(t *testing.T) {
	d := &trustDeps{stubDeps: &stubDeps{panes: map[string]string{"tmux-1": claudeTrustPane(t)}}, dropKeys: true}
	p := newTrustPoller(t, d)
	p.SetTrustWorkspace(true)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}

	for i := 0; i < trustMaxAttempts+3; i++ {
		p.tryTrustPrompt(context.Background(), s, d.panes["tmux-1"])
	}

	seq := d.sentSequence("tmux-1")
	require.NotContains(t, seq, "Enter")
	require.Len(t, seq, trustMaxAttempts, "one Down per attempt, capped")
	require.Empty(t, d.recordedEvents("agent-1"))
}

// TestTrustPromptIgnoresOtherPrompts: an ordinary permission prompt is not the
// trust step's business.
func TestTrustPromptIgnoresOtherPrompts(t *testing.T) {
	d := &stubDeps{}
	p := newTrustPoller(t, d)
	p.SetTrustWorkspace(true)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}
	p.tryTrustPrompt(context.Background(), s, "Do you want to proceed?\n ❯ 1. Yes\n   2. No")
	require.Equal(t, 0, d.sendCount())
}

// TestTrustPromptOwnership: with trust_workspace on the auto-approve path leaves
// the trust dialog to the tick (no double answer); with it off the dialog is an
// ordinary sticky prompt, answered only under allow_sticky — and then with the
// cursor keys, not a digit.
func TestTrustPromptOwnership(t *testing.T) {
	d := &trustDeps{stubDeps: &stubDeps{panes: map[string]string{"tmux-1": claudeTrustPane(t)}}}
	p := newTrustPoller(t, d)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1"}

	p.SetTrustWorkspace(true)
	p.AutoApprovePolicy = approval.Policy{Enabled: true, AllowSticky: true}
	p.tryAutoApprove(context.Background(), s, d.panes["tmux-1"])
	require.Equal(t, 0, d.sendCount())

	p.SetTrustWorkspace(false)
	p.AutoApprovePolicy = approval.Policy{Enabled: true}
	p.tryAutoApprove(context.Background(), s, d.panes["tmux-1"])
	require.Equal(t, 0, d.sendCount(), "sticky grant needs allow_sticky")

	p.AutoApprovePolicy = approval.Policy{Enabled: true, AllowSticky: true}
	p.tryAutoApprove(context.Background(), s, d.panes["tmux-1"])
	require.Equal(t, []string{"Down", "Enter"}, d.sentSequence("tmux-1"))
}
