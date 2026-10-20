package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/stretchr/testify/require"
)

// resumableRejecting is the TEST-ONLY detector backend for the relaunch paths:
// a codex adapter whose resume command names the model and which needs no
// transcript, so Restore/SwitchRole/hot-swap reach the supervised relaunch.
type resumableRejecting struct{ rejectingCodex }

func (r resumableRejecting) Capabilities() agentbackend.Caps {
	c := r.rejectingCodex.Capabilities()
	c.StructuredTranscript = false
	return c
}

func (r resumableRejecting) ResumeCmd(o agentbackend.ResumeOpts) (string, bool) {
	return "codex resume --last -m " + o.Model, true
}

// recordedRig is a recovery rig whose codex adapter is resumableRejecting.
func newRecordedRig(t *testing.T, notPrePrompt bool, script map[string]paneScript) (*recoveryRig, *agentstore.Agent) {
	t.Helper()
	r := newRecoveryRig(t, []string{"rec-model", "alt-model"}, script)
	orig, err := agentbackend.Get("codex") // the rejectingCodex registered by the rig
	require.NoError(t, err)
	rc := orig.(rejectingCodex)
	rc.notPrePrompt = notPrePrompt
	agentbackend.Register(resumableRejecting{rc})
	r.run.noSession = true
	a := &agentstore.Agent{ID: "rec-agent", TmuxSession: "rec-agent", AiCli: "codex", Model: "rec-model", Workdir: r.cwd, PermissionMode: "workspace-write"}
	return r, a
}

func requireRecordedRejection(t *testing.T, r *recoveryRig, a *agentstore.Agent, err error) {
	t.Helper()
	var rej *ModelRejectedError
	require.True(t, errors.As(err, &rej), "a recorded model that is rejected is attention-needed, got %v", err)
	require.Equal(t, "rec-model", rej.Model)
	require.Equal(t, []string{"rec-model"}, r.launched(), "no retry and no substitution")
	require.Equal(t, []string{"codex/rec-model"}, r.rec.calls, "negative evidence recorded once")
	require.Equal(t, "rec-model", a.Model, "the record is never rewritten to another model")
	require.GreaterOrEqual(t, r.run.kills["rec-agent"], 1, "failed child and tmux shell reaped")
	var logged bool
	for _, e := range a.Events {
		if e.Type == "model_launch_attempt" {
			logged = true
		}
	}
	require.True(t, logged, "attempt audit event persisted on the record")
}

func TestRestoreRecordedModelRejectedIsAttentionNeededNoSubstitution(t *testing.T) {
	r, a := newRecordedRig(t, false, map[string]paneScript{"rec-model": rejected("rec-model"), "alt-model": {pane: "codex ready"}})
	err := r.lc.Restore(context.Background(), a)
	requireRecordedRejection(t, r, a, err)
}

func TestSwitchRoleRecordedModelRejectedIsAttentionNeededNoSubstitution(t *testing.T) {
	r, a := newRecordedRig(t, false, map[string]paneScript{"rec-model": rejected("rec-model"), "alt-model": {pane: "codex ready"}})
	err := r.lc.SwitchRole(context.Background(), a)
	requireRecordedRejection(t, r, a, err)
}

func TestRestoreWithoutAttestationStillSupervisedBecausePromptIsWithheld(t *testing.T) {
	// A resume command carries no task prompt, so the pre-prompt attestation is not
	// needed: this is the prompt-withheld path.
	r, a := newRecordedRig(t, true, map[string]paneScript{"rec-model": rejected("rec-model")})
	err := r.lc.Restore(context.Background(), a)
	requireRecordedRejection(t, r, a, err)
}

func TestRestoreHealthyLaunchIsUntouched(t *testing.T) {
	r, a := newRecordedRig(t, false, map[string]paneScript{"rec-model": {pane: "codex ready"}})
	require.NoError(t, r.lc.Restore(context.Background(), a))
	require.Empty(t, r.rec.calls)
	require.Equal(t, []string{"rec-model"}, r.launched())
}

func TestHotSwapSuccessorRejectedLeavesRecordIntactAndTyped(t *testing.T) {
	r, a := newRecordedRig(t, false, map[string]paneScript{"alt-model": rejected("alt-model")})
	a.AiCli, a.Model = "claude", "sonnet" // current driver; successor is codex/alt-model
	b, err := agentbackend.Get("codex")
	require.NoError(t, err)
	hf := filepath.Join(r.cwd, "handoff.md")
	require.NoError(t, os.WriteFile(hf, []byte("h"), 0o600))
	err = r.lc.launchSuccessor(context.Background(), a, b, "alt-model", "workspace-write", hf, handoff.Handoff{Goal: "g"}, SwapRequest{})
	var rej *ModelRejectedError
	require.True(t, errors.As(err, &rej), "got %v", err)
	require.Equal(t, "alt-model", rej.Model, "judged against the successor's model, not the record's")
	require.Equal(t, "claude", a.AiCli, "record only mutated by HotSwap after success")
	require.Equal(t, "sonnet", a.Model)
	require.Equal(t, []string{"alt-model"}, r.launched(), "no retry, no substitution")
	require.Equal(t, []string{"codex/alt-model"}, r.rec.calls)
}
