package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

// Regression validation for the backend-safe relaunch plan: every existing-agent
// relaunch seam (restore, switch-role, hot-swap, fork) must launch with a mode the
// target backend accepts, never wider than the stored intent, and must leave the
// stored mode and audit untouched when the launch fails or is refused.

// storedModeCorpus is the backend's own modes plus the values that have gone wrong
// historically: foreign vocabulary at each intent, the legacy values the backends'
// tables omit, an unknown string, and "" (nothing stored).
func storedModeCorpus(b agentbackend.Backend) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append(append([]string{}, b.Capabilities().PermissionModes...),
		"", "acceptEdits", "bypassPermissions", "dontAsk", "plan", "auto", "force", "workspace-write", "wat") {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func relaunchAgent(t *testing.T, backend, mode, role string) (*FakeRunner, *Lifecycle, *agentstore.Agent, *[]ModeNormalization) {
	t.Helper()
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t agent-v": {Err: errStub("no session")},
	}}
	lc := New(fr, &FakeConfig{})
	lc.ProjectsDir = t.TempDir()
	var got []ModeNormalization
	lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }
	return fr, lc, &agentstore.Agent{
		ID: "agent-v", TmuxSession: "agent-v", AiCli: backend, Workdir: t.TempDir(),
		PermissionMode: mode, Role: role,
	}, &got
}

func newSessionCalls(fr *FakeRunner) int {
	n := 0
	for _, c := range fr.Calls {
		if len(c.Argv) > 1 && c.Argv[0] == "tmux" && c.Argv[1] == "new-session" {
			n++
		}
	}
	return n
}

func failSendKeys(argv []string) error {
	if len(argv) > 1 && argv[0] == "tmux" && argv[1] == "send-keys" {
		return errors.New("send-keys boom")
	}
	return nil
}

// TestRelaunchSeams_LifecycleMatrix drives Restore and SwitchRole for every
// backend × stored-mode pair and asserts the outcome contract.
func TestRelaunchSeams_LifecycleMatrix(t *testing.T) {
	paths := map[string]func(*Lifecycle, *agentstore.Agent) error{
		"restore":     func(l *Lifecycle, a *agentstore.Agent) error { return l.Restore(context.Background(), a) },
		"switch-role": func(l *Lifecycle, a *agentstore.Agent) error { return l.SwitchRole(context.Background(), a) },
	}
	for path, run := range paths {
		for _, id := range agentbackend.IDs() {
			b := mustBackend(t, id)
			if _, ok := b.(agentbackend.PermissionMapper); !ok {
				continue
			}
			for _, mode := range storedModeCorpus(b) {
				for _, role := range []string{"", "planner"} {
					name := path + "/" + id + "/" + mode + "/" + role
					t.Run(name, func(t *testing.T) {
						t.Parallel() // each case owns its runner; the relaunch path settles ~100ms
						fr, lc, sess, got := relaunchAgent(t, id, mode, role)
						err := run(lc, sess)
						if errors.Is(err, ErrNoSafeMode) {
							// Refused: stored record untouched, nothing launched, no audit.
							require.Equal(t, mode, sess.PermissionMode)
							require.Empty(t, *got)
							require.Zero(t, newSessionCalls(fr), "refusal must not create a tmux session")
							return
						}
						if err != nil {
							// A backend with no resume support is not a mode concern.
							require.NotContains(t, err.Error(), "permission", "unexpected mode error: %v", err)
							require.Empty(t, *got)
							require.Equal(t, mode, sess.PermissionMode)
							return
						}
						if len(*got) == 0 {
							// Kept verbatim: the stored mode must be a valid native mode.
							require.Equal(t, mode, sess.PermissionMode)
							require.True(t, mode == "" || agentbackend.ModeAccepted(b, mode), "kept an invalid mode %q", mode)
							return
						}
						require.Len(t, *got, 1)
						n := (*got)[0]
						require.Equal(t, path, n.Path)
						require.Equal(t, mode, n.From)
						require.True(t, agentbackend.ModeAccepted(b, n.To), "launched %q not accepted by %s", n.To, id)
						require.True(t, n.Rationale.AcceptedIntent.AtMost(n.Rationale.StoredIntent),
							"%s widened %s -> %s", name, n.Rationale.StoredIntent, n.Rationale.AcceptedIntent)
						require.NotEmpty(t, n.Rationale.Note(), "a changed mode must carry an audit note")
						if mode == "" {
							require.False(t, n.Persist, "an empty stored mode keeps tracking config")
							require.Empty(t, sess.PermissionMode)
						} else {
							require.True(t, n.Persist)
							require.Equal(t, n.To, sess.PermissionMode)
						}
					})
				}
			}
		}
	}
}

// Invalid legacy modes get a translation audit, and a failed launch of the same
// relaunch preserves both the stored mode and the (absent) audit.
func TestRelaunch_LaunchFailurePreservesStoredModeAndAudit(t *testing.T) {
	cases := []struct{ backend, stored string }{
		{"cursor", "acceptEdits"},
		{"cursor", "wat"},
		{"codex", "default"},
		{"goose", "acceptEdits"},
		{"aider", "acceptEdits"},
		{"claude", "force"},
	}
	for _, c := range cases {
		t.Run(c.backend+"/"+c.stored, func(t *testing.T) {
			fr, lc, sess, got := relaunchAgent(t, c.backend, c.stored, "")
			fr.FailIf = failSendKeys
			require.Error(t, lc.Restore(context.Background(), sess))
			require.Error(t, lc.SwitchRole(context.Background(), sess))
			require.Empty(t, *got)
			require.Equal(t, c.stored, sess.PermissionMode)
		})
	}
}

// Restrictive intent with no representable mode on the target is rejected, not widened.
func TestRelaunch_UnrepresentableRestrictiveIntentRefused(t *testing.T) {
	// aider/crush only know default + skip-all; claude "plan" has no safe rendering.
	for _, backend := range []string{"aider", "crush"} {
		t.Run(backend, func(t *testing.T) {
			lc, fr, sess := newSwapLC(t)
			sess.PermissionMode = "plan"
			writeClaudeTranscript(t, lc, sess)
			var got []ModeNormalization
			lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }

			_, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: backend, Model: "m", Reason: SwapReasonManual})
			// aider/crush may map plan → default? the resolver decides; whichever it
			// does, it must never land on a skip-all mode.
			if err != nil {
				require.ErrorIs(t, err, ErrNoSafeMode)
				require.Equal(t, "claude", sess.AiCli, "refused swap must not change the backend")
				require.Equal(t, "plan", sess.PermissionMode)
				require.Empty(t, got)
				for _, c := range fr.Calls {
					require.False(t, len(c.Argv) > 1 && c.Argv[1] == "kill-session", "refusal must leave the live agent running")
				}
				return
			}
			require.True(t, sess.PermissionMode != "yolo" && sess.PermissionMode != "yes-always", "widened to %q", sess.PermissionMode)
		})
	}
}

// Hot-swap launch failure keeps backend, model and stored mode, and emits no audit.
func TestHotSwap_LaunchFailureKeepsModeAndAudit(t *testing.T) {
	lc, fr, sess := newSwapLC(t)
	sess.PermissionMode = "acceptEdits"
	writeClaudeTranscript(t, lc, sess)
	var got []ModeNormalization
	lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }
	fr.FailIf = failSendKeys

	_, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "cursor", Model: "m", Reason: SwapReasonManual})
	require.Error(t, err)
	require.Equal(t, "claude", sess.AiCli)
	require.Equal(t, "acceptEdits", sess.PermissionMode)
	require.Empty(t, got)
}

// Cross-backend swap: translated/stepped-down modes are audited after success and
// never exceed the stored intent.
func TestHotSwap_TranslationAudit(t *testing.T) {
	cases := []struct {
		stored, to string
		wantMode   string
		outcome    RelaunchOutcome
	}{
		{"bypassPermissions", "crush", "yolo", OutcomeTranslated},
		{"acceptEdits", "cursor", "default", OutcomeSteppedDown},
		{"dontAsk", "cursor", "ask", OutcomeTranslated},
		{"default", "codex", "read-only", OutcomeSteppedDown},
		{"auto", "goose", "approve", OutcomeSteppedDown},
	}
	for _, c := range cases {
		t.Run(c.stored+"->"+c.to, func(t *testing.T) {
			lc, _, sess := newSwapLC(t)
			sess.PermissionMode = c.stored
			writeClaudeTranscript(t, lc, sess)
			var got []ModeNormalization
			lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }

			res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: c.to, Model: "m", Reason: SwapReasonManual})
			require.NoError(t, err)
			require.Equal(t, c.wantMode, res.ToMode)
			require.Equal(t, c.wantMode, sess.PermissionMode)
			require.Len(t, got, 1)
			n := got[0]
			require.Equal(t, "hot-swap", n.Path)
			require.Equal(t, c.stored, n.From)
			require.Equal(t, c.outcome, n.Rationale.Outcome)
			require.True(t, n.Rationale.AcceptedIntent.AtMost(n.Rationale.StoredIntent))
			require.Equal(t, c.to, n.Rationale.TargetBackend)
		})
	}
}

// Role/config fallbacks: empty stored mode never yields skip-all unless the role or
// config asked for it, and a role may only tighten.
func TestResolveRelaunchMode_RoleConfigFallbacks(t *testing.T) {
	cases := []struct {
		name, backend, stored, role, def string
		maxIntent                        agentbackend.PermissionIntent
	}{
		{"empty/no config on goose", "goose", "", "", "", agentbackend.IntentDefault},
		{"empty/claude config auto on goose is not skip-all", "goose", "", "", "auto", agentbackend.IntentAcceptEdits},
		{"empty/bypass config on aider", "aider", "", "", "bypassPermissions", agentbackend.IntentSkipAll},
		{"planner stored skip-all on cursor", "cursor", "force", "planner", "", agentbackend.IntentPlan},
		{"planner stored skip-all on codex", "codex", "danger-full-access", "planner", "", agentbackend.IntentReadOnly},
		{"worker empty stays at worker posture", "codex", "", "worker", "default", agentbackend.IntentAcceptEdits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := mustBackend(t, c.backend)
			got, rat, err := ResolveRelaunchMode(RelaunchModeInput{
				StoredMode: c.stored, StoredBackend: b, Target: b, Role: c.role, DefaultMode: c.def,
			})
			if errors.Is(err, ErrNoSafeMode) {
				return
			}
			require.NoError(t, err)
			require.True(t, agentbackend.ModeAccepted(b, got))
			require.True(t, rat.AcceptedIntent.AtMost(c.maxIntent), "%s: %s > %s", c.name, rat.AcceptedIntent, c.maxIntent)
		})
	}
}

// Fork: a fork with no explicit mode inherits the source's stored mode through the
// resolver; it must not fall to the (Claude-vocabulary) config default.
func TestSpawnFork_InheritsSourceModeNeverWider(t *testing.T) {
	cases := []struct {
		name, srcMode, want string
	}{
		{"read-only source stays read-only", "read-only", "read-only"},
		{"workspace-write kept", "workspace-write", "workspace-write"},
		{"foreign claude plan -> read-only", "plan", "read-only"},
		{"unknown stored -> not skip-all", "wat", "read-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lc, fr := newForkLC()
			lc.SetConfig(&FakeConfig{PermissionMode: "bypassPermissions"})
			s, err := lc.Spawn(context.Background(), SpawnRequest{
				Type: store.TypeDevelopment, Ticket: "fork-m", Repo: "/repo", Backend: "codex",
				Model: "qwen2.5-coder:3b", ForkFrom: "src-agent",
				ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
				ForkSourceBranch:    "src-branch", ForkSourceMode: c.srcMode,
			})
			require.NoError(t, err)
			require.Equal(t, c.want, s.PermissionMode)
			launch := forkLaunchLine(t, fr, "fork-m")
			require.NotContains(t, launch, "danger-full-access", "fork must not inherit the config default's skip-all")
		})
	}
}

// An explicit mode on the fork request is the operator's choice and is kept.
func TestSpawnFork_ExplicitModeWins(t *testing.T) {
	lc, _ := newForkLC()
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-e", Repo: "/repo", Backend: "codex",
		Model: "qwen2.5-coder:3b", ForkFrom: "src-agent", PermissionMode: "workspace-write",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch", ForkSourceMode: "read-only",
	})
	require.NoError(t, err)
	require.Equal(t, "workspace-write", s.PermissionMode)
}

// A role default must not skip inheritance: a worker/autopilot fork of a read-only
// source stays read-only; the role only ever tightens.
func TestSpawnFork_RoleDefaultsNeverWidenSource(t *testing.T) {
	cases := []struct{ role, src, want string }{
		{"worker", "read-only", "read-only"},
		{"autopilot", "read-only", "read-only"},
		{"planner", "workspace-write", "read-only"},
		{"worker", "workspace-write", "workspace-write"},
		{"worker", "", "workspace-write"},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.src, func(t *testing.T) {
			lc, _ := newForkLC()
			s, err := lc.Spawn(context.Background(), SpawnRequest{
				Type: store.TypeDevelopment, Ticket: "fork-r", Repo: "/repo", Backend: "codex",
				Model: "qwen2.5-coder:3b", Role: c.role, ForkFrom: "src-agent",
				ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
				ForkSourceBranch:    "src-branch", ForkSourceMode: c.src,
			})
			require.NoError(t, err)
			require.Equal(t, c.want, s.PermissionMode)
		})
	}
}

// A translated fork mode is audited only after the caller stores the record, once.
func TestSpawnFork_NormalizationAuditedAfterStore(t *testing.T) {
	lc, _ := newForkLC()
	var got []ModeNormalization
	lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-a", Repo: "/repo", Backend: "codex",
		Model: "qwen2.5-coder:3b", ForkFrom: "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch", ForkSourceMode: "plan", // claude vocabulary on a codex source
	})
	require.NoError(t, err)
	require.Empty(t, got, "nothing is emitted before the record is stored")
	lc.EmitForkNormalization(context.Background(), s.ID)
	lc.EmitForkNormalization(context.Background(), s.ID)
	require.Len(t, got, 1, "emitted exactly once")
	n := got[0]
	require.Equal(t, "fork", n.Path)
	require.Equal(t, "plan", n.From)
	require.Equal(t, "read-only", n.To)
	require.False(t, n.Persist)
	require.True(t, n.Rationale.AcceptedIntent.AtMost(n.Rationale.StoredIntent))
	require.NotEmpty(t, n.Rationale.Note())
}

func TestSpawnFork_LaunchFailureEmitsNothing(t *testing.T) {
	lc, fr := newForkLC()
	var got []ModeNormalization
	lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }
	fr.FailIf = failSendKeys
	_, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "fork-f", Repo: "/repo", Backend: "codex",
		Model: "qwen2.5-coder:3b", ForkFrom: "src-agent",
		ForkSourceSessionID: "11111111-2222-3333-4444-555555555555",
		ForkSourceBranch:    "src-branch", ForkSourceMode: "plan",
	})
	require.Error(t, err)
	lc.EmitForkNormalization(context.Background(), "fork-f")
	require.Empty(t, got)
}
