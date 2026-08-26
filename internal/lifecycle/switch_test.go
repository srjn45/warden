package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register claude + codex
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// nonAlnumRe mirrors the claude adapter's project-dir encoding so a test can place a
// transcript where the retiring backend will look for it.
var nonAlnumRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// stubResolver is a test double for SuccessorResolver returning a fixed resolution.
type stubResolver struct {
	res *router.Resolution
	err error
}

func (s stubResolver) ResolveTier(_ context.Context, _ backendstore.ModelTier) (*router.Resolution, error) {
	return s.res, s.err
}

func (s stubResolver) Resolve(_ context.Context, _ router.ResolveOptions) (*router.Resolution, error) {
	return s.res, s.err
}

// newSwapLC builds a Lifecycle wired for hot-swap tests: a real temp worktree (the
// os.Stat guard requires it to exist), a ProjectsDir for the retiring backend's
// transcript, and prompt/exit dirs. It returns the lifecycle, the fake runner, the
// session, and the worktree path.
func newSwapLC(t *testing.T) (*Lifecycle, *FakeRunner, *store.Session) {
	t.Helper()
	repo := t.TempDir()
	projects := t.TempDir()

	fr := &FakeRunner{Responses: map[string]FakeResp{}}
	lc := New(fr, &FakeConfig{})
	lc.ProjectsDir = projects
	lc.PromptsDir = filepath.Join(t.TempDir(), "prompts")

	sess := &store.Session{
		ID:              "agent-swap1",
		TmuxSession:     "agent-swap1",
		Backend:         "claude",
		Model:           "opus",
		ClaudeSessionID: "11111111-2222-3333-4444-555555555555",
		Repo:            repo,
		Workdir:         repo,
		Branch:          "feat/x",
		Worktree:        ".worktrees/x",
	}
	return lc, fr, sess
}

// writeClaudeTranscript drops a minimal JSONL transcript where the claude adapter
// resolves it for sess, so HotSwap's extractor has real turns to distil.
func writeClaudeTranscript(t *testing.T, lc *Lifecycle, sess *store.Session) {
	t.Helper()
	dir := filepath.Join(lc.ProjectsDir, nonAlnumRe.ReplaceAllString(sess.Workdir, "-"))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"Implement the CSV export feature."}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I will use a streaming parser."},{"type":"tool_use","name":"Write","input":{"file_path":"export/csv.go"}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Next step: wire up the --format flag."}]}}`,
	}
	path := filepath.Join(dir, sess.ClaudeSessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

// swapLaunchLine returns the send-keys payload typed into the successor's pane.
func swapLaunchLine(t *testing.T, fr *FakeRunner, id string) string {
	t.Helper()
	for _, c := range fr.Calls {
		if len(c.Argv) >= 5 && c.Argv[0] == "tmux" && c.Argv[1] == "send-keys" && c.Argv[3] == id {
			return c.Argv[4]
		}
	}
	t.Fatalf("no tmux send-keys launch recorded for %q", id)
	return ""
}

// TestHotSwapExplicitBackend is the core path: swap claude→codex, verify the handoff
// file is written with the distilled context, the old CLI is retired, the successor
// is launched in the same worktree, and the session is mutated.
func TestHotSwapExplicitBackend(t *testing.T) {
	lc, fr, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)

	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{
		Backend: "codex", Model: "gpt-5-codex", Reason: SwapReasonManual,
	})
	require.NoError(t, err)

	// Handoff persisted with distilled transcript context.
	require.Equal(t, filepath.Join(sess.Workdir, ".warden", "handoff-agent-swap1.md"), res.HandoffPath)
	body, err := os.ReadFile(res.HandoffPath)
	require.NoError(t, err)
	md := string(body)
	require.Contains(t, md, "Implement the CSV export feature.", "goal distilled from the first user turn")
	require.Contains(t, md, "I will use a streaming parser.", "decision distilled")
	require.Contains(t, md, "export/csv.go", "modified file distilled")
	require.Contains(t, md, "Next step: wire up the --format flag.", "next step distilled")
	require.Contains(t, md, "claude (opus) → codex (gpt-5-codex)", "swap direction in system context")

	// Old CLI retired: kill-session on the retiring tmux session.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "agent-swap1"})

	// Successor launched in the SAME worktree: new-session with -c <workdir>.
	requireNewSessionInWorkdir(t, fr, sess.ID, sess.Workdir)

	// Launch line is codex, carrying the continuation prompt (file-backed).
	launch := swapLaunchLine(t, fr, sess.ID)
	require.True(t, strings.HasPrefix(launch, "codex "), "successor launches codex: %q", launch)
	require.Contains(t, launch, lc.PromptsDir, "continuation prompt is file-backed into the launch line")

	// Session mutated to the new driver.
	require.Equal(t, "codex", sess.Backend)
	require.Equal(t, "gpt-5-codex", sess.Model)
	require.Equal(t, "codex", res.ToBackend)
	require.Equal(t, "claude", res.FromBackend)
	require.False(t, res.ResolverUsed)
}

// TestHotSwapContinuationPromptContent: the seeded prompt orients the successor —
// names the handoff file, restates the goal and next step.
func TestHotSwapContinuationPromptContent(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)

	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "codex"})
	require.NoError(t, err)

	prompt := continuationPrompt(res.HandoffPath, res.Handoff, SwapRequest{Backend: "codex"})
	require.Contains(t, prompt, res.HandoffPath)
	require.Contains(t, prompt, "Implement the CSV export feature.")
	require.Contains(t, prompt, "wire up the --format flag")
	require.Contains(t, prompt, "taking over an in-progress warden session")
}

// TestHotSwapModelOnlyKeepsBackend: a swap with only a Model set is a same-backend
// model bump — the backend is preserved.
func TestHotSwapModelOnlyKeepsBackend(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)

	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Model: "sonnet"})
	require.NoError(t, err)
	require.Equal(t, "claude", res.ToBackend, "model-only swap keeps the current backend")
	require.Equal(t, "sonnet", sess.Model)
	require.False(t, res.ResolverUsed)
}

// TestHotSwapTierResolvesViaRouter: a Tier request routes through the resolver and
// adopts the resolved backend/model.
func TestHotSwapTierResolvesViaRouter(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)
	lc.Resolver = stubResolver{res: &router.Resolution{BackendID: "codex", ModelID: "o1", Tier: backendstore.Tier1}}

	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Tier: backendstore.Tier1, Reason: SwapReasonQuota})
	require.NoError(t, err)
	require.True(t, res.ResolverUsed)
	require.Equal(t, "codex", res.ToBackend)
	require.Equal(t, "o1", res.ToModel)
	require.Equal(t, "codex", sess.Backend)
	require.Equal(t, SwapReasonQuota, res.Reason)
}

// TestHotSwapTierWithoutResolver: asking for tier resolution with no resolver wired
// is refused (not a silent guess), and the current agent is left untouched.
func TestHotSwapTierWithoutResolver(t *testing.T) {
	lc, fr, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)

	_, err := lc.HotSwap(context.Background(), sess, SwapRequest{Tier: backendstore.Tier1})
	require.ErrorIs(t, err, ErrNoResolver)
	require.Equal(t, "claude", sess.Backend, "session untouched on resolve failure")
	// The retiring CLI must NOT have been killed (resolution runs first).
	require.NotContains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "agent-swap1"})
}

// TestHotSwapNoTarget: a request naming no successor at all is refused.
func TestHotSwapNoTarget(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)
	_, err := lc.HotSwap(context.Background(), sess, SwapRequest{})
	require.ErrorIs(t, err, ErrNoSwapTarget)
}

// TestHotSwapMissingWorkdir: a gone worktree is refused up front.
func TestHotSwapMissingWorkdir(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	sess.Workdir = filepath.Join(sess.Workdir, "does-not-exist")
	_, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "codex"})
	require.ErrorIs(t, err, ErrWorkdirMissing)
}

// TestHotSwapAbsentTranscriptStillSwaps: with no transcript, extraction yields an
// empty (but valid) handoff and the swap still completes — the successor gets the
// system context even when the transcript is gone.
func TestHotSwapAbsentTranscriptStillSwaps(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	// No transcript written.
	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "codex"})
	require.NoError(t, err)
	body, err := os.ReadFile(res.HandoffPath)
	require.NoError(t, err)
	require.Contains(t, string(body), "_No explicit goal was recorded", "empty handoff renders placeholders")
	require.Equal(t, "codex", sess.Backend)
}

// TestHotSwapFreshSessionIDForPinningSuccessor: swapping TO claude (a pinning
// backend) mints a fresh session id (a new conversation, not a resume).
func TestHotSwapFreshSessionIDForPinningSuccessor(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	sess.Backend = "codex" // retiring backend
	old := sess.ClaudeSessionID
	res, err := lc.HotSwap(context.Background(), sess, SwapRequest{Backend: "claude", Model: "opus"})
	require.NoError(t, err)
	require.Equal(t, "claude", res.ToBackend)
	require.NotEmpty(t, sess.ClaudeSessionID)
	require.NotEqual(t, old, sess.ClaudeSessionID, "a pinning successor gets a fresh minted session id")
}

// requireNewSessionInWorkdir asserts a `tmux new-session … -s <id> … -c <workdir>`
// was issued for the successor.
func requireNewSessionInWorkdir(t *testing.T, fr *FakeRunner, id, workdir string) {
	t.Helper()
	for _, c := range fr.Calls {
		if len(c.Argv) < 3 || c.Argv[0] != "tmux" || c.Argv[1] != "new-session" {
			continue
		}
		var hasID, hasDir bool
		for i, a := range c.Argv {
			if a == "-s" && i+1 < len(c.Argv) && c.Argv[i+1] == id {
				hasID = true
			}
			if a == "-c" && i+1 < len(c.Argv) && c.Argv[i+1] == workdir {
				hasDir = true
			}
		}
		if hasID && hasDir {
			return
		}
	}
	t.Fatalf("no `tmux new-session -s %s … -c %s` recorded", id, workdir)
}
