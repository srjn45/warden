package lifecycle

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register claude + aider
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestRestoreRefusesNonResumableBackend verifies the !Caps.Resume degradation
// (design §5): a backend that can't resume by id (Aider) is refused by Restore
// with a clear "start fresh" message rather than building a wrong resume command.
func TestRestoreRefusesNonResumableBackend(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	err := lc.Restore(context.Background(), &store.Session{
		ID: "a1", TmuxSession: "a1", Backend: "aider", ClaudeSessionID: "irrelevant", Workdir: t.TempDir(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support")
}

// TestSystemPromptHintsSkippedForAider verifies the SystemPromptInject=false
// degradation: warden's pipeline/collab/git hints are silently dropped for a
// backend that has no system-prompt injection, so no invalid flags reach it.
func TestSystemPromptHintsSkippedForAider(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	aider, err := agentbackend.Get("aider")
	require.NoError(t, err)

	require.Equal(t, "", lc.pipelineHint(aider))
	require.Equal(t, "", lc.collabHint(aider))
	require.Equal(t, "", lc.gitConventionsHint(aider))

	// Claude (the default) still emits its --append-system-prompt fragments.
	require.Contains(t, lc.collabHint(agentbackend.Default()), "--append-system-prompt")
}

// TestSpawnAiderLaunchString locks the full command typed into tmux for an
// Aider spawn: a valid bare interactive `aider` invocation (the prompt is typed in
// after launch via PromptSeeder, not carried on the launch line) that is free of
// Claude-only flags (--append-system-prompt, --session-id, --settings). This is the
// regression guard that `wd start --backend aider` actually launches.
func TestSpawnAiderLaunchString(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	lc := New(fr, &FakeConfig{})
	lc.PromptsDir = "/state/prompts"
	s, err := lc.Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "PROJ-9", Repo: "/repo",
		Backend: "aider", Model: "ollama_chat/qwen2.5-coder:3b", Prompt: "implement add",
	})
	require.NoError(t, err)
	require.Equal(t, "aider", s.Backend)

	want := "aider --no-show-model-warnings --model 'ollama_chat/qwen2.5-coder:3b' --yes-always"
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, want, "Enter"})
	require.NotContains(t, want, "--message", "prompt is seeded after launch, not on the launch line")
	require.NotContains(t, want, "--append-system-prompt", "no Claude system-prompt flags reach Aider")
	require.NotContains(t, want, "--session-id", "Aider has no assignable session id")
	require.NotContains(t, want, "--settings", "Claude-only guard hooks are skipped")
}

// TestSpawnSessionIDMintGatedByCapability proves Spawn mints a warden session id
// ONLY for a pinning backend (Claude, SessionIDControl=true) and leaves it empty
// for a non-pinning backend (Codex). The empty id is the dir-scoped transcript
// fallback; the poller discovers-then-pins the agent's real id post-launch (§5.2).
func TestSpawnSessionIDMintGatedByCapability(t *testing.T) {
	newLC := func() *Lifecycle {
		fr := &FakeRunner{Responses: map[string]FakeResp{
			"git worktree list --porcelain": {Out: noOtherWorktrees},
		}}
		lc := New(fr, &FakeConfig{})
		lc.PromptsDir = "/state/prompts"
		return lc
	}

	// Non-pinning backend (Codex) ⇒ id left empty for discover-then-pin.
	codex, err := newLC().Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "CDX-1", Repo: "/repo", Backend: "codex",
	})
	require.NoError(t, err)
	require.Empty(t, codex.ClaudeSessionID, "non-pinning backend leaves the session id empty")

	// Pinning backend (Claude default) ⇒ a UUID minted at spawn (unchanged).
	claude, err := newLC().Spawn(context.Background(), SpawnRequest{
		Type: store.TypeDevelopment, Ticket: "CLD-1", Repo: "/repo",
	})
	require.NoError(t, err)
	require.NotEmpty(t, claude.ClaudeSessionID, "pinning backend mints a session id at spawn (regression-lock)")
}

// TestLaunchModelPassthroughForNonClaude verifies model resolution is
// backend-aware: Claude expands aliases/defaults; Aider (BYO model) receives the
// raw model and an empty model stays empty (Aider supplies its own default).
func TestLaunchModelPassthroughForNonClaude(t *testing.T) {
	lc := New(&FakeRunner{}, &FakeConfig{})
	aider, err := agentbackend.Get("aider")
	require.NoError(t, err)

	require.Equal(t, "ollama_chat/qwen2.5-coder:3b", lc.launchModel(aider, "ollama_chat/qwen2.5-coder:3b"))
	require.Equal(t, "", lc.launchModel(aider, ""), "empty stays empty for BYO-model backend")

	// Claude still applies its alias/default expansion (never empty).
	require.NotEqual(t, "", lc.launchModel(agentbackend.Default(), ""))
}
