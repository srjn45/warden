package backends

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// --- Command builders -------------------------------------------------------

func TestCodexLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + default mode (Codex's own posture applies)",
			opts: agentbackend.LaunchOpts{Model: "qwen2.5-coder:3b", Mode: "default"},
			want: "codex -m 'qwen2.5-coder:3b'",
		},
		{
			name: "read-only sandbox passes through",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "read-only"},
			want: "codex -m 'm' -s read-only",
		},
		{
			name: "workspace-write sandbox passes through",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "workspace-write"},
			want: "codex -m 'm' -s workspace-write",
		},
		{
			name: "danger-full-access also pins -a never",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "danger-full-access"},
			want: "codex -m 'm' -s danger-full-access -a never",
		},
		{
			name: "claude 'dangerously-skip-permissions' folds onto danger-full-access",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "dangerously-skip-permissions"},
			want: "codex -m 'm' -s danger-full-access -a never",
		},
		{
			name: "empty model omits -m (BYO config provider)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "codex",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "uuid", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "codex -m 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Codex{}.LaunchCmd(tt.opts))
		})
	}
}

// TestCodexLaunchQuotesModel is the command-injection guard: a model string with
// shell metacharacters must be single-quoted so it cannot break out of the line
// typed into a tmux pane.
func TestCodexLaunchQuotesModel(t *testing.T) {
	got := Codex{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "codex -m 'm; touch /tmp/pwned #'", got)
}

func TestCodexResumeCmd(t *testing.T) {
	// Codex mints its own UUID id warden cannot pin, so resume is dir-scoped --last.
	cmd, ok := Codex{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "whatever", Model: "m"})
	require.True(t, ok, "Codex supports resume (Caps.Resume=true)")
	require.Equal(t, "codex resume --last", cmd)
}

func TestCodexLaunchPromptArg(t *testing.T) {
	got := Codex{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` "$(cat '/state/prompts/job-1')"`, got)
}

// TestCodexImplementsSessionForker locks the seam: Codex exposes `codex fork`, so
// it must be type-assertable as a SessionForker for the lifecycle fork branch.
func TestCodexImplementsSessionForker(t *testing.T) {
	_, ok := agentbackend.Backend(Codex{}).(agentbackend.SessionForker)
	require.True(t, ok, "Codex forks sessions via `codex fork`")
}

// TestCodexForkCmd checks the fork launch-line shaping: the EXPLICIT source UUID is
// always quoted (never `--last`, §4.3), and -m/-s/-a map exactly as LaunchCmd. An
// empty source id returns ok=false so the caller reports a clean "cannot fork"
// instead of launching a bare `codex fork`.
func TestCodexForkCmd(t *testing.T) {
	tests := []struct {
		name   string
		opts   agentbackend.ForkOpts
		want   string
		wantOK bool
	}{
		{
			name:   "empty source id ⇒ not forkable",
			opts:   agentbackend.ForkOpts{Model: "m", Mode: "default"},
			want:   "",
			wantOK: false,
		},
		{
			name:   "explicit id + model, default mode (codex posture applies)",
			opts:   agentbackend.ForkOpts{SourceSessionID: "11111111-2222-3333-4444-555555555555", Model: "qwen2.5-coder:3b", Mode: "default"},
			want:   "codex fork '11111111-2222-3333-4444-555555555555' -m 'qwen2.5-coder:3b'",
			wantOK: true,
		},
		{
			name:   "workdir pins -C right after the id (suppresses the cross-cwd picker)",
			opts:   agentbackend.ForkOpts{SourceSessionID: "id", Workdir: "/repo/.worktrees/fork-1", Model: "m", Mode: "workspace-write"},
			want:   "codex fork 'id' -C '/repo/.worktrees/fork-1' -m 'm' -s workspace-write",
			wantOK: true,
		},
		{
			name:   "read-only sandbox passes through",
			opts:   agentbackend.ForkOpts{SourceSessionID: "id", Model: "m", Mode: "read-only"},
			want:   "codex fork 'id' -m 'm' -s read-only",
			wantOK: true,
		},
		{
			name:   "danger-full-access also pins -a never",
			opts:   agentbackend.ForkOpts{SourceSessionID: "id", Model: "m", Mode: "danger-full-access"},
			want:   "codex fork 'id' -m 'm' -s danger-full-access -a never",
			wantOK: true,
		},
		{
			name:   "empty model omits -m (BYO config provider)",
			opts:   agentbackend.ForkOpts{SourceSessionID: "id", Mode: "default"},
			want:   "codex fork 'id'",
			wantOK: true,
		},
		{
			name:   "Name is ignored (codex mints its own id)",
			opts:   agentbackend.ForkOpts{SourceSessionID: "id", Name: "fork-1", Mode: "default"},
			want:   "codex fork 'id'",
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Codex{}.ForkCmd(tt.opts)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestCodexForkQuotesSourceID is the command-injection guard for the fork source id:
// a UUID positional carrying shell metacharacters must be single-quoted so it cannot
// break out of the line typed into a tmux pane.
func TestCodexForkQuotesSourceID(t *testing.T) {
	got, ok := Codex{}.ForkCmd(agentbackend.ForkOpts{SourceSessionID: "id'; touch /tmp/pwned #", Mode: "default"})
	require.True(t, ok)
	require.Equal(t, `codex fork 'id'\''; touch /tmp/pwned #'`, got)
}

// TestCodexForkNeverUsesLast guards §4.3: the fork must pass the explicit source id,
// never `--last` (which is cwd-scoped and would miss the fork's separate worktree).
func TestCodexForkNeverUsesLast(t *testing.T) {
	got, ok := Codex{}.ForkCmd(agentbackend.ForkOpts{SourceSessionID: "id", Mode: "default"})
	require.True(t, ok)
	require.NotContains(t, got, "--last")
	require.Contains(t, got, "codex fork 'id'")
}

func TestCodexHeadlessCmd(t *testing.T) {
	argv, ok := Codex{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "classify this"}, argv)
}

// --- Transcript resolution (dir-scoped) -------------------------------------

// TestCodexTranscriptPathDirScoped points CODEX_HOME at the fixture session tree
// and resolves the rollout by matching session_meta.cwd to the agent's workdir.
func TestCodexTranscriptPathDirScoped(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	p, ok := Codex{}.TranscriptPath("", "/work/agent-codex", "warden-placeholder-uuid")
	require.True(t, ok, "rollout for the workdir resolves")
	require.True(t, strings.HasSuffix(p, ".jsonl"))
	require.Contains(t, p, filepath.Join("sessions", "2026", "06", "28"))
}

func TestCodexTranscriptPathDegrades(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	// No workdir ⇒ nothing to resolve.
	_, ok := Codex{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// A directory with no matching rollout ⇒ degrade (digest shows "no transcript").
	_, ok = Codex{}.TranscriptPath("", "/work/no-such-agent", "")
	require.False(t, ok)

	// A CODEX_HOME with no sessions tree at all ⇒ degrade, no error.
	t.Setenv("CODEX_HOME", t.TempDir())
	_, ok = Codex{}.TranscriptPath("", "/work/agent-codex", "")
	require.False(t, ok)
}

// --- Discover-then-pin session id -------------------------------------------

// TestCodexDiscoverSessionID points CODEX_HOME at the fixture tree and asserts the
// dir-scoped locator finds the agent's rollout and extracts its minted session id
// from the `session_meta` header — the id warden pins post-launch (discover-then-pin).
func TestCodexDiscoverSessionID(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	id, ok := Codex{}.DiscoverSessionID("", "/work/agent-codex")
	require.True(t, ok, "session id discovered from the workdir's rollout")
	require.Equal(t, "019f0d8c-6c54-7471-af1c-a9043b9f11e0", id)
}

// TestCodexDiscoverSessionIDDegrades covers the misses: no workdir, a workdir with
// no rollout, and a sessions tree that exists but whose header carries no id — each
// returns ok=false so the poller keeps the empty id and retries on a later tick.
func TestCodexDiscoverSessionIDDegrades(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	_, ok := Codex{}.DiscoverSessionID("", "")
	require.False(t, ok, "no workdir ⇒ nothing to resolve")

	_, ok = Codex{}.DiscoverSessionID("", "/work/no-such-agent")
	require.False(t, ok, "no rollout for the dir ⇒ degrade")

	// A rollout whose session_meta header carries no id ⇒ degrade rather than pin "".
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "06", "28")
	require.NoError(t, os.MkdirAll(day, 0o755))
	roll := filepath.Join(day, "rollout-2026-06-28T00-00-00-noid.jsonl")
	require.NoError(t, os.WriteFile(roll,
		[]byte(`{"type":"session_meta","payload":{"cwd":"/work/noid"}}`+"\n"), 0o644))
	t.Setenv("CODEX_HOME", home)
	_, ok = Codex{}.DiscoverSessionID("", "/work/noid")
	require.False(t, ok, "header without a session id ⇒ degrade")
}

// codexRolloutSessionID also accepts the older rollouts that carry only `id` (no
// `session_id`) in the header.
func TestCodexDiscoverSessionIDLegacyIDField(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "06", "28")
	require.NoError(t, os.MkdirAll(day, 0o755))
	roll := filepath.Join(day, "rollout-2026-06-28T00-00-00-legacy.jsonl")
	require.NoError(t, os.WriteFile(roll,
		[]byte(`{"type":"session_meta","payload":{"id":"legacy-uuid-1234","cwd":"/work/legacy"}}`+"\n"), 0o644))
	t.Setenv("CODEX_HOME", home)

	id, ok := Codex{}.DiscoverSessionID("", "/work/legacy")
	require.True(t, ok)
	require.Equal(t, "legacy-uuid-1234", id)
}

// --- Transcript parsing -----------------------------------------------------

// TestCodexParseTranscript parses the real captured rollout fixture and asserts the
// neutral Turns warden's digest depends on: exactly one human prompt and one model
// reply, with Codex's synthetic <environment_context>/developer messages dropped.
func TestCodexParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "codex", "sessions", "2026", "06", "28",
		"rollout-2026-06-28T11-25-34-019f0d8c-6c54-7471-af1c-a9043b9f11e0.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Codex{}.ParseTranscript(f)
	require.NoError(t, err)

	var users, assistants int
	for _, tr := range turns {
		switch tr.Role {
		case "user":
			users++
		case "assistant":
			assistants++
		}
	}
	require.Equal(t, 1, users, "synthetic env-context / developer user messages are dropped")
	require.Equal(t, 1, assistants)

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "add(a, b)")
	require.False(t, turns[0].Timestamp.IsZero(), "rollout timestamp applied")

	require.Equal(t, "assistant", turns[1].Role)
	require.Contains(t, turns[1].Text, `"name": "add"`)
}

// TestCodexParseTranscriptToolCall covers a function_call (apply_patch): the tool
// name and the patched file fold onto the preceding assistant turn. The 3B local
// fixture model never emitted a tool call, so this fixture is hand-authored from
// the Codex rollout schema (mirroring OpenCode's export-tool.json).
func TestCodexParseTranscriptToolCall(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "codex", "rollout-toolcall.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Codex{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 2, "the function_call folds onto the assistant turn; output is ignored")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "subtract")

	a := turns[1]
	require.Equal(t, "assistant", a.Role)
	require.Contains(t, a.Text, "I'll add a subtract")
	require.Equal(t, "apply_patch", a.ToolName)
	require.Equal(t, []string{"calc.py"}, a.Files, "files parsed from the apply_patch envelope")
}

// TestCodexParseTranscriptShellPatch covers the shell-tool route: an apply_patch
// run via the `shell` tool's command array still yields the touched file.
func TestCodexParseTranscriptShellPatch(t *testing.T) {
	stream := strings.Join([]string{
		`{"timestamp":"2026-06-28T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"edit calc.py"}]}}`,
		`{"timestamp":"2026-06-28T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"apply_patch <<'EOF'\\n*** Begin Patch\\n*** Update File: calc.py\\n@@\\n+x=1\\n*** End Patch\\nEOF\"]}"}}`,
	}, "\n")

	turns, err := Codex{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "assistant", turns[1].Role, "a tool call with no preceding assistant text starts a new turn")
	require.Equal(t, "shell", turns[1].ToolName)
	require.Equal(t, []string{"calc.py"}, turns[1].Files)
}

// TestCodexParseTranscriptTolerant skips malformed lines and ignores
// event_msg/turn_context/session_meta records rather than erroring.
func TestCodexParseTranscriptTolerant(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"type":"session_meta","payload":{"cwd":"/x"}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-06-28T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		``,
	}, "\n")

	turns, err := Codex{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Equal(t, "hello", turns[0].Text)
}

// --- State / approval (live markers) ----------------------------------------

// codexFixture reads a captured tmux-pane fixture from testdata/codex/.
func codexFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex", name))
	require.NoError(t, err)
	return string(b)
}

// TestCodexDetectState classifies each captured pane: streaming ⇒ Working (the
// "esc to interrupt" marker), an open approval ⇒ NeedsInput, an at-rest pane ⇒
// Unknown (Codex has no positive idle marker; warden infers idle from staleness).
func TestCodexDetectState(t *testing.T) {
	tests := []struct {
		fixture string
		want    agentbackend.State
	}{
		{"state-working.txt", agentbackend.StateWorking},
		{"approval-command.txt", agentbackend.StateNeedsInput},
		{"state-idle.txt", agentbackend.StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			require.Equal(t, tt.want, Codex{}.DetectState(codexFixture(t, tt.fixture)))
		})
	}

	// An unrecognized pane stays Unknown (no false positive).
	require.Equal(t, agentbackend.StateUnknown, Codex{}.DetectState("just some quiet output"))
}

// TestCodexParseApproval parses the captured command-escalation approval into the
// neutral Approval: the proposed command (Action), the "Would you like to …?"
// header (Question), the three options top-down (1-indexed), the highlighted
// option (SelectedIdx), and the least-privilege non-sticky "Yes" (AffirmativeIdx).
func TestCodexParseApproval(t *testing.T) {
	a, ok := Codex{}.ParseApproval(codexFixture(t, "approval-command.txt"))
	require.True(t, ok, "the captured approval prompt parses")

	require.Equal(t, "curl -sI https://example.com", a.Action)
	require.Equal(t, "Would you like to run the following command?", a.Question)
	require.Equal(t, []string{
		"Yes, proceed (y)",
		"Yes, and don't ask again for commands that start with `curl -sI` (p)",
		"No, and tell Codex what to do differently (esc)",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the › cursor sits on option 1")
	require.Equal(t, 1, a.AffirmativeIdx, "least-privilege affirmative is the non-sticky Yes")
	require.False(t, a.AffirmativeSticky, "option 1 is a one-shot grant, not a standing one")
}

// TestCodexParseApprovalNegative proves a non-approval pane (idle or working) is
// NOT mis-read as an approval — the header gate keeps the auto-approve path honest.
func TestCodexParseApprovalNegative(t *testing.T) {
	for _, name := range []string{"state-idle.txt", "state-working.txt"} {
		t.Run(name, func(t *testing.T) {
			_, ok := Codex{}.ParseApproval(codexFixture(t, name))
			require.False(t, ok)
		})
	}

	// A bare numbered list in agent prose (no "Would you like to" header) is not an
	// approval, even though it has sequential 1..N lines.
	prose := "Here are the steps:\n  1. Yes do this\n  2. No skip that\n"
	_, ok := Codex{}.ParseApproval(prose)
	require.False(t, ok, "a numbered list without the approval header is not a prompt")
}

// TestCodexAffirmativeStickyFallback covers the case where the only affirmative is
// a standing "don't ask again" grant: it is chosen with sticky=true.
func TestCodexAffirmativeStickyFallback(t *testing.T) {
	idx, sticky := codexAffirmative([]string{
		"Yes, and don't ask again for this session",
		"No, stop",
	})
	require.Equal(t, 1, idx)
	require.True(t, sticky)

	idx, sticky = codexAffirmative([]string{"No, cancel", "No, and tell Codex what to do"})
	require.Equal(t, 0, idx, "no affirmative offered")
	require.False(t, sticky)
}

// --- Capabilities / pricing -------------------------------------------------

func TestCodexCapabilities(t *testing.T) {
	c := Codex{}.Capabilities()
	require.True(t, c.Resume, "Codex resumes (codex resume --last)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript, "Tier A: rollout JSONL parses into Turns")
	require.False(t, c.SessionIDControl, "Codex mints its own UUID session id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"read-only", "workspace-write", "danger-full-access"}, c.PermissionModes)
}

func TestCodexNoPricing(t *testing.T) {
	_, ok := Codex{}.Pricing()
	require.False(t, ok, "OSS/BYO ⇒ no warden-side dollar pricing table yet")
}

func TestCodexSystemPromptUnsupported(t *testing.T) {
	_, ok := Codex{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

// --- Context injection (AGENTS.md) ------------------------------------------

// TestCodexImplementsContextInjector locks that Codex carries the optional seam
// (the lifecycle type-assert keys off this), unlike a flag-based backend.
func TestCodexImplementsContextInjector(t *testing.T) {
	_, ok := agentbackend.Backend(Codex{}).(agentbackend.ContextInjector)
	require.True(t, ok, "Codex injects context via AGENTS.md")
}

// TestCodexInjectContextWritesBlock verifies a fresh workdir gets an AGENTS.md
// carrying warden's addendum inside the delimited block.
func TestCodexInjectContextWritesBlock(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Codex{}.InjectContext(dir, "warden coordination hints"))

	got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	require.NoError(t, err)
	s := string(got)
	require.Contains(t, s, "<!-- warden:begin -->")
	require.Contains(t, s, "<!-- warden:end -->")
	require.Contains(t, s, "warden coordination hints")
}

// TestCodexInjectContextIdempotent verifies a second call replaces the warden block
// in place rather than appending a duplicate.
func TestCodexInjectContextIdempotent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Codex{}.InjectContext(dir, "first hints"))
	require.NoError(t, Codex{}.InjectContext(dir, "second hints"))

	got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	require.NoError(t, err)
	s := string(got)
	require.Equal(t, 1, strings.Count(s, "<!-- warden:begin -->"), "no duplicate warden block")
	require.Equal(t, 1, strings.Count(s, "<!-- warden:end -->"))
	require.Contains(t, s, "second hints")
	require.NotContains(t, s, "first hints", "stale warden block replaced in place")
}

// TestCodexInjectContextPreservesUserFile verifies a user's pre-existing AGENTS.md
// content survives: only the warden block is added/refreshed around it.
func TestCodexInjectContextPreservesUserFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("# My project rules\nAlways run the linter.\n"), 0o644))

	require.NoError(t, Codex{}.InjectContext(dir, "warden hints"))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(got)
	require.Contains(t, s, "# My project rules", "user content preserved")
	require.Contains(t, s, "Always run the linter.")
	require.Contains(t, s, "warden hints")

	// A second inject still preserves the user content and doesn't duplicate the block.
	require.NoError(t, Codex{}.InjectContext(dir, "warden hints v2"))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	s = string(got)
	require.Contains(t, s, "# My project rules")
	require.Equal(t, 1, strings.Count(s, "<!-- warden:begin -->"))
	require.Contains(t, s, "warden hints v2")
	require.NotContains(t, s, "warden hints\n", "old warden text replaced")
}

// TestCodexInjectContextNoOps verifies an empty workdir or empty text writes nothing.
func TestCodexInjectContextNoOps(t *testing.T) {
	require.NoError(t, Codex{}.InjectContext("", "hints"))

	dir := t.TempDir()
	require.NoError(t, Codex{}.InjectContext(dir, "   \n  "))
	_, err := os.Stat(filepath.Join(dir, "AGENTS.md"))
	require.True(t, os.IsNotExist(err), "empty text writes no file")
}

// TestCodexInjectContextGitExclude verifies the dropped AGENTS.md is added to the
// repo's info/exclude (so it never lands in the agent's diff), idempotently.
func TestCodexInjectContextGitExclude(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755))

	require.NoError(t, Codex{}.InjectContext(dir, "hints"))
	require.NoError(t, Codex{}.InjectContext(dir, "hints again"))

	excl, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(excl), "AGENTS.md"), "excluded once, not duplicated")
}

func TestCodexRegistered(t *testing.T) {
	b, err := agentbackend.Get("codex")
	require.NoError(t, err)
	require.Equal(t, "Codex", b.DisplayName())
	require.Equal(t, "codex", b.Binary())
}

// --- Native review (codex review → wd review) -------------------------------

// TestCodexImplementsReviewer locks that Codex carries the optional Reviewer seam
// (the `wd review` type-assert keys off this), unlike Claude which has no native
// review subcommand.
func TestCodexImplementsReviewer(t *testing.T) {
	_, ok := agentbackend.Backend(Codex{}).(agentbackend.Reviewer)
	require.True(t, ok, "Codex exposes `codex review` natively")
}

// TestCodexReviewCmd checks the argv shaping for both diff scopes (the PR-A prose
// form) and the optional review prompt. SchemaFile is "" here — PR-A always asks for
// the prose review; the structured `--output-schema` form is covered separately.
func TestCodexReviewCmd(t *testing.T) {
	cases := []struct {
		name string
		opts agentbackend.ReviewOpts
		want []string
	}{
		{
			name: "uncommitted default",
			opts: agentbackend.ReviewOpts{Scope: "uncommitted"},
			want: []string{"codex", "review", "--uncommitted"},
		},
		{
			name: "empty scope defaults to uncommitted",
			opts: agentbackend.ReviewOpts{},
			want: []string{"codex", "review", "--uncommitted"},
		},
		{
			name: "base branch scope",
			opts: agentbackend.ReviewOpts{Scope: "base", Base: "main"},
			want: []string{"codex", "review", "--base", "main"},
		},
		{
			name: "base scope without a branch falls back to uncommitted",
			opts: agentbackend.ReviewOpts{Scope: "base"},
			want: []string{"codex", "review", "--uncommitted"},
		},
		{
			name: "extra prompt rides as the trailing positional",
			opts: agentbackend.ReviewOpts{Scope: "uncommitted", Prompt: "focus on the auth path"},
			want: []string{"codex", "review", "--uncommitted", "focus on the auth path"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, ok := Codex{}.ReviewCmd(tc.opts)
			require.True(t, ok, "Codex always offers native review")
			require.Equal(t, tc.want, argv)
		})
	}
}

// TestCodexReviewCmdStructuredForm locks the verified structured form (PR-B1): a
// Structured request switches to the NON-INTERACTIVE `codex exec review` sub-form whose
// native review output persists to the rollout for ParseReviewResult to read back. It
// carries NO `--output-schema`: verified against codex v0.142.3, the review subcommand
// ignores a caller schema (only plain `codex exec` honors `--output-schema`), so warden
// requests no schema and normalizes codex's own `review_output` instead. The prose form
// (Structured=false) stays byte-identical to PR-A.
func TestCodexReviewCmdStructuredForm(t *testing.T) {
	cases := []struct {
		name string
		opts agentbackend.ReviewOpts
		want []string
	}{
		{
			name: "structured uncommitted",
			opts: agentbackend.ReviewOpts{Scope: "uncommitted", Structured: true},
			want: []string{"codex", "exec", "review", "--uncommitted"},
		},
		{
			name: "structured base branch",
			opts: agentbackend.ReviewOpts{Scope: "base", Base: "main", Structured: true},
			want: []string{"codex", "exec", "review", "--base", "main"},
		},
		{
			name: "structured carries an extra prompt",
			opts: agentbackend.ReviewOpts{Scope: "uncommitted", Structured: true, Prompt: "focus on auth"},
			want: []string{"codex", "exec", "review", "--uncommitted", "focus on auth"},
		},
		{
			name: "prose form unchanged (no exec, no schema)",
			opts: agentbackend.ReviewOpts{Scope: "uncommitted"},
			want: []string{"codex", "review", "--uncommitted"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, ok := Codex{}.ReviewCmd(tc.opts)
			require.True(t, ok)
			require.Equal(t, tc.want, argv)
			require.NotContains(t, argv, "--output-schema", "codex review ignores a caller schema; warden must not emit one")
		})
	}
}

// TestCodexImplementsStructuredReviewer asserts the optional StructuredReviewer seam is
// wired so `wd review --json` lights up for Codex.
func TestCodexImplementsStructuredReviewer(t *testing.T) {
	_, ok := agentbackend.Backend(Codex{}).(agentbackend.StructuredReviewer)
	require.True(t, ok, "Codex exposes its native review_output via StructuredReviewer")
}

// TestCodexParseReviewResultEmpty reads the AUTHENTIC $0-local capture (an
// `exited_review_mode` review_output with no findings — the 7B model judged the patch
// correct, missing the planted bug; design §5 "$0 proves plumbing not accuracy"). It
// proves the locate→parse→normalize plumbing on a real rollout.
func TestCodexParseReviewResultEmpty(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))
	rf, ok, err := Codex{}.ParseReviewResult("/work/agent-review-clean")
	require.NoError(t, err)
	require.True(t, ok, "the rollout carries an exited_review_mode review_output")
	require.Equal(t, "patch is correct", rf.Verdict)
	require.Contains(t, rf.Summary, "do not contain any issues")
	require.NotNil(t, rf.Findings, "findings must marshal as [] not null")
	require.Empty(t, rf.Findings)
}

// TestCodexParseReviewResultFindings exercises the finding mapping against a
// schema-faithful fixture (codex's verified native review_output field names —
// title/body/priority/code_location.line_range.start_line; the $0 models won't reliably
// emit findings, so this mirrors the adapter's schema-faithful tool-call fixture). It
// checks abs-path→repo-relative, start_line→line, and priority→severity folding.
func TestCodexParseReviewResultFindings(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))
	rf, ok, err := Codex{}.ParseReviewResult("/work/agent-review-findings")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "patch is incorrect", rf.Verdict)
	require.Len(t, rf.Findings, 3)

	require.Equal(t, "calc.go", rf.Findings[0].File, "absolute path relativized to the workdir")
	require.Equal(t, 4, rf.Findings[0].Line, "start_line becomes the neutral line")
	require.Equal(t, "error", rf.Findings[0].Severity, "priority 0 → error")
	require.Equal(t, "Possible divide-by-zero", rf.Findings[0].Title)
	require.Contains(t, rf.Findings[0].Message, "panic")

	require.Equal(t, "warning", rf.Findings[1].Severity, "priority 1 → warning")
	require.Equal(t, "info", rf.Findings[2].Severity, "priority ≥2 → info")
}

// TestCodexParseReviewResultDegrades covers the misses: no workdir and a workdir with no
// rollout both return ok=false with no error, so `wd review --json` reports a clean "no
// structured review output" rather than crashing.
func TestCodexParseReviewResultDegrades(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	_, ok, err := Codex{}.ParseReviewResult("")
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = Codex{}.ParseReviewResult("/work/no-such-agent")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestCodexLastReviewOutputNoEvent: a rollout with conversation but no
// exited_review_mode event yields ok=false (not an error) — the structured path then
// degrades cleanly.
func TestCodexLastReviewOutputNoEvent(t *testing.T) {
	rollout := `{"type":"session_meta","payload":{"cwd":"/work/x"}}
{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`
	_, ok, err := codexLastReviewOutput(strings.NewReader(rollout))
	require.NoError(t, err)
	require.False(t, ok)
}

// TestCodexPriorityToSeverity locks the neutral severity folding of codex's integer
// priority (0 = highest).
func TestCodexPriorityToSeverity(t *testing.T) {
	require.Equal(t, "error", codexPriorityToSeverity(0))
	require.Equal(t, "warning", codexPriorityToSeverity(1))
	require.Equal(t, "info", codexPriorityToSeverity(2))
	require.Equal(t, "info", codexPriorityToSeverity(5))
}

// TestCodexReviewFixture documents the captured $0-local `codex review --uncommitted`
// run (testdata/codex/review-uncommitted.txt): it shows the verb→adapter→review
// run→stream plumbing producing a real reviewer verdict on the Ollama rig. The
// fixture proves the plumbing, not review quality on a tiny local model (design §5).
func TestCodexReviewFixture(t *testing.T) {
	out := codexFixture(t, "review-uncommitted.txt")
	require.Contains(t, out, "provider: ollama", "captured against the $0-local Ollama rig")
	require.Contains(t, out, "codex", "carries the reviewer's response section")
}
