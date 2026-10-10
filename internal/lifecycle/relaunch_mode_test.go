package lifecycle

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends"
)

func TestResolveRelaunchMode_Table(t *testing.T) {
	cases := []struct {
		name         string
		stored, from string
		to, role     string
		def          string
		want         string
		outcome      RelaunchOutcome
		intent       agentbackend.PermissionIntent
	}{
		{"same backend keeps native", "acceptEdits", "claude", "claude", "", "", "acceptEdits", OutcomeKept, agentbackend.IntentAcceptEdits},
		{"same backend keeps legacy auto", "auto", "claude", "claude", "worker", "", "auto", OutcomeKept, agentbackend.IntentAcceptEdits},
		{"codex keeps workspace-write", "workspace-write", "codex", "codex", "", "", "workspace-write", OutcomeKept, agentbackend.IntentAcceptEdits},
		{"foreign force on claude never skip-all-widened", "force", "claude", "claude", "", "", "bypassPermissions", OutcomeTranslated, agentbackend.IntentSkipAll},
		{"foreign read-only on claude -> plan", "read-only", "claude", "claude", "", "", "plan", OutcomeTranslated, agentbackend.IntentPlan},
		{"unknown on claude -> default", "wat", "claude", "claude", "", "", "default", OutcomeTranslated, agentbackend.IntentDefault},
		{"dontAsk to cursor stays read-only", "dontAsk", "claude", "cursor", "", "", "ask", OutcomeTranslated, agentbackend.IntentReadOnly},
		{"claude default to codex steps down", "default", "claude", "codex", "", "", "read-only", OutcomeSteppedDown, agentbackend.IntentReadOnly},
		{"claude acceptEdits to cursor steps down", "acceptEdits", "claude", "cursor", "", "", "default", OutcomeSteppedDown, agentbackend.IntentDefault},
		{"claude acceptEdits to aider steps down", "acceptEdits", "claude", "aider", "", "", "default", OutcomeSteppedDown, agentbackend.IntentDefault},
		{"claude bypass to crush", "bypassPermissions", "claude", "crush", "", "", "yolo", OutcomeTranslated, agentbackend.IntentSkipAll},
		{"goose auto is skip-all", "auto", "goose", "claude", "", "", "bypassPermissions", OutcomeTranslated, agentbackend.IntentSkipAll},
		{"claude auto to goose is not skip-all", "auto", "claude", "goose", "", "", "approve", OutcomeSteppedDown, agentbackend.IntentDefault},
		{"agy sandbox -> default not skip", "sandbox", "antigravity", "antigravity", "", "", "sandbox", OutcomeKept, agentbackend.IntentDefault},
		{"empty uses config default", "", "claude", "claude", "", "acceptEdits", "acceptEdits", OutcomeDefaulted, agentbackend.IntentAcceptEdits},
		{"empty config auto on codex", "", "codex", "codex", "", "auto", "workspace-write", OutcomeDefaulted, agentbackend.IntentAcceptEdits},
		{"empty no config -> default", "", "claude", "claude", "", "", "default", OutcomeDefaulted, agentbackend.IntentDefault},
		{"empty uses role default", "", "codex", "codex", "worker", "plan", "workspace-write", OutcomeDefaulted, agentbackend.IntentAcceptEdits},
		{"planner tightens stored skip-all", "bypassPermissions", "claude", "claude", "planner", "", "plan", OutcomeTranslated, agentbackend.IntentPlan},
		{"worker does not widen default", "default", "claude", "claude", "worker", "", "default", OutcomeKept, agentbackend.IntentDefault},
		{"autopilot does not widen plan", "plan", "claude", "claude", "autopilot", "", "plan", OutcomeKept, agentbackend.IntentPlan},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rat, err := ResolveRelaunchMode(RelaunchModeInput{
				StoredMode: c.stored, StoredBackend: mustBackend(t, c.from), Target: mustBackend(t, c.to),
				Role: c.role, DefaultMode: c.def,
			})
			require.NoError(t, err)
			require.Equal(t, c.want, got)
			require.Equal(t, c.outcome, rat.Outcome)
			require.Equal(t, c.intent, rat.AcceptedIntent)
			require.True(t, rat.AcceptedIntent.AtMost(rat.StoredIntent))
			require.True(t, agentbackend.ModeAccepted(mustBackend(t, c.to), got))
			if c.outcome == OutcomeKept {
				require.Empty(t, rat.Note())
			} else {
				require.NotEmpty(t, rat.Note())
			}
		})
	}
}

// Every accepted mode of every backend, relaunched on every backend, must be
// accepted by the target and never exceed the stored intent.
func TestResolveRelaunchMode_NonEscalationMatrix(t *testing.T) {
	ids := agentbackend.IDs()
	require.NotEmpty(t, ids)
	for _, from := range ids {
		fb := mustBackend(t, from)
		for _, mode := range fb.Capabilities().PermissionModes {
			for _, to := range ids {
				tb := mustBackend(t, to)
				for _, role := range []string{"", "planner", "worker", "autopilot"} {
					got, rat, err := ResolveRelaunchMode(RelaunchModeInput{StoredMode: mode, StoredBackend: fb, Target: tb, Role: role})
					if errors.Is(err, ErrNoSafeMode) {
						continue
					}
					require.NoError(t, err, "%s/%s -> %s role=%q", from, mode, to, role)
					require.True(t, agentbackend.ModeAccepted(tb, got), "%s/%s -> %s: %q not accepted", from, mode, to, got)
					require.True(t, rat.AcceptedIntent.AtMost(rat.StoredIntent), "%s/%s -> %s role=%q: %s > %s", from, mode, to, role, rat.AcceptedIntent, rat.StoredIntent)
					back, _, ok := classifyOwn(tb, got)
					require.True(t, ok, "%s/%s -> %s: %q unclassifiable", from, mode, to, got)
					require.True(t, back.AtMost(rat.StoredIntent))
				}
			}
		}
	}
}

func TestResolveRelaunchMode_Errors(t *testing.T) {
	_, _, err := ResolveRelaunchMode(RelaunchModeInput{StoredMode: "default"})
	require.Error(t, err)
}

func TestResolveRelaunchMode_NoMutation(t *testing.T) {
	in := RelaunchModeInput{StoredMode: "force", StoredBackend: mustBackend(t, "claude"), Target: mustBackend(t, "claude")}
	a, _, _ := ResolveRelaunchMode(in)
	b, _, _ := ResolveRelaunchMode(in)
	require.Equal(t, a, b)
	require.Equal(t, "force", in.StoredMode)
}
