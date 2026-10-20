package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/stretchr/testify/require"
)

// seamCodex is the TEST-ONLY prompt-withheld Codex: the fake detector plus the
// REAL Codex InputReady. (rejectingCodex embeds only the Backend interface, so it
// has no InputReady and keeps the legacy prompt-on-launch-line path.)
type seamCodex struct{ rejectingCodex }

func (seamCodex) InputReady(pane string) bool { return (backends.Codex{}).InputReady(pane) }

// codexReadyPane satisfies the real Codex positive input-ready check.
const codexReadyPane = "OpenAI Codex\n› \n  m · ~/work\n"

func newSeamRig(t *testing.T, models []string, script map[string]paneScript) *recoveryRig {
	t.Helper()
	r := newRecoveryRig(t, models, script)
	orig, err := agentbackend.Get("codex")
	require.NoError(t, err)
	agentbackend.Register(seamCodex{orig.(rejectingCodex)})
	return r
}

func TestSeamPromptWithheldThenDeliveredOnceAfterInputReady(t *testing.T) {
	r := newSeamRig(t, []string{"m-ok"}, map[string]paneScript{"m-ok": {pane: codexReadyPane}})
	got, err := r.spawn("m-ok")
	require.NoError(t, err)
	require.Equal(t, "m-ok", got)
	require.Len(t, r.run.lines, 1)
	require.NotContains(t, r.run.lines[0], "$(cat", "the task prompt is held out of the launch line")
	require.Equal(t, 1, r.run.pastes, "delivered exactly once")
	require.Equal(t, []string{"launch:m-ok", "paste"}, r.run.events, "only after the CLI is input-ready")
}

func TestSeamRejectedAttemptNeverReceivesThePrompt(t *testing.T) {
	r := newSeamRig(t, []string{"gpt-5.5", "m-c"}, map[string]paneScript{
		"gpt-5.5": rejected("gpt-5.5"), "m-c": {pane: codexReadyPane},
	})
	got, err := r.spawn("")
	require.NoError(t, err)
	require.Equal(t, "m-c", got)
	require.Equal(t, []string{"launch:gpt-5.5", "launch:m-c", "paste"}, r.run.events)
	require.Equal(t, 1, r.run.pastes)
}

func TestSeamAmbiguousTimeoutIsAttentionNeededNoRetryNoPrompt(t *testing.T) {
	r := newSeamRig(t, []string{"m-a", "m-b"}, map[string]paneScript{
		"m-a": {pane: "booting\n"}, "m-b": {pane: codexReadyPane},
	})
	_, err := r.spawn("m-a")
	var amb *StartupAmbiguousError
	require.True(t, errors.As(err, &amb), "got %v", err)
	require.True(t, IsModelRecoveryError(err))
	require.Equal(t, 0, r.run.pastes, "an ambiguous launch never gets the prompt")
	require.Equal(t, []string{"m-a"}, r.launched(), "no retry, no substitution")
	require.Empty(t, r.rec.calls, "ambiguity is not negative model evidence")
	require.GreaterOrEqual(t, r.run.kills[firstSession(r)], 1, "child and tmux shell reaped")
}

func TestSeamUnexplainedExitIsAmbiguousNotInvalidModel(t *testing.T) {
	r := newSeamRig(t, []string{"m-a"}, map[string]paneScript{
		"m-a": {pane: "error: 429 rate limited for model m-a\n", exit: 1},
	})
	_, err := r.spawn("m-a")
	var amb *StartupAmbiguousError
	require.True(t, errors.As(err, &amb), "got %v", err)
	require.Equal(t, 0, r.run.pastes)
	require.Empty(t, r.rec.calls)
}

func TestSeamPromptDeliveryFailureIsAmbiguousAndNeverRepeated(t *testing.T) {
	r := newSeamRig(t, []string{"m-ok"}, map[string]paneScript{"m-ok": {pane: codexReadyPane}})
	r.run.failPaste = true
	_, err := r.spawn("m-ok")
	var amb *StartupAmbiguousError
	require.True(t, errors.As(err, &amb), "got %v", err)
	require.True(t, amb.PromptDelivered)
	require.Equal(t, []string{"m-ok"}, r.launched())
}

func TestSeamCancelBeforeReadyIsAmbiguous(t *testing.T) {
	r := newSeamRig(t, []string{"m-a"}, map[string]paneScript{"m-a": {pane: "booting\n"}})
	b, err := agentbackend.Get("codex")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _ := r.lc.awaitStartup(ctx, &agentstore.Agent{ID: "x", AiCli: "codex", Model: "m-a"}, "x", b.(agentbackend.LaunchRejectionDetector), "", b.(agentbackend.InputReadiness))
	require.Equal(t, outcomeAmbiguous, out)
}

func TestSeamNotUsedWithoutADetector(t *testing.T) {
	r := newSeamRig(t, []string{"m-ok"}, map[string]paneScript{"m-ok": {pane: "codex ready"}})
	orig, err := agentbackend.Get("codex")
	require.NoError(t, err)
	agentbackend.Register(orig.(seamCodex).Backend) // the plain adapter: no detector
	_, err = r.spawn("m-ok")
	require.NoError(t, err)
	require.Len(t, r.run.lines, 1)
	require.Contains(t, r.run.lines[0], "$(cat", "today's behaviour: prompt on the launch line")
	require.Equal(t, 0, r.run.pastes)
}

func firstSession(r *recoveryRig) string {
	r.run.mu.Lock()
	defer r.run.mu.Unlock()
	for k := range r.run.launch {
		return k
	}
	return ""
}

func TestAdoptRecordedModelRejectedIsAttentionNeeded(t *testing.T) {
	r, _ := newRecordedRig(t, false, map[string]paneScript{"rec-model": rejected("rec-model")})
	rb, err := agentbackend.Get("codex")
	require.NoError(t, err)
	r.lc.backend = mappedRejecting{rb.(resumableRejecting)}
	_, err = r.lc.Adopt(context.Background(), AdoptRequest{ID: "adopted", Cwd: r.cwd, AICLISessionID: "sid", Model: "rec-model"})
	var rej *ModelRejectedError
	require.True(t, errors.As(err, &rej), "got %v", err)
	require.Equal(t, []string{"rec-model"}, r.launched())
}

// mappedRejecting adds Codex's real intent->mode mapping to the fake-detector
// backend, which Adopt's relaunch-mode resolver needs (the config default is
// Claude vocabulary and must be rendered for the launched backend).
type mappedRejecting struct{ resumableRejecting }

func (mappedRejecting) ModeIntent(mode string) (agentbackend.PermissionIntent, bool) {
	return (backends.Codex{}).ModeIntent(mode)
}

func (mappedRejecting) ModeForIntent(i agentbackend.PermissionIntent) (string, bool) {
	return (backends.Codex{}).ModeForIntent(i)
}
