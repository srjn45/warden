# Deterministic Claude Session ID Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pin each agent's Claude conversation to a stored UUID (`Session.ClaudeSessionID`) so the transcript file is deterministic (`<uuid>.jsonl`) and the session is resumable by id later.

**Architecture:** Generate a v4 UUID (`crypto/rand`, no new dep) at spawn, store it on `Session`, and launch `claude … --session-id <uuid> --name <agent-id>` on both spawn paths. Replace the "newest `.jsonl`" transcript guess with a session-id lookup (encoded-dir → unique glob), falling back to the legacy newest-`.jsonl` only when the id is empty. Surface the id in `agentctl status`.

**Tech Stack:** Go 1.26, stdlib (`crypto/rand`, `encoding/hex`, `path/filepath`), testify.

**Design spec:** `docs/superpowers/specs/2026-06-02-agentctl-session-id-design.md`

**Verified (empirical):** `claude --session-id <uuid> --name <id> -p` exits 0 and writes the transcript to exactly `<uuid>.jsonl`; both flags are accepted together.

**Ordering:** store field + UUID helper first (Task 1, no consumers), then spawn pins it + existing-test updates (Task 2), then the transcript lookup rewrite (Task 3), then the CLI surface (Task 4). Build stays green at every commit. The daemon API / `internal/client` / MCP / web GUI need **no** change — they serialize the whole `Session`, so the new field flows automatically.

---

### Task 1: `store` — `ClaudeSessionID` field + `NewSessionID()`

**Files:**
- Modify: `internal/store/types.go`
- Create: `internal/store/id.go`
- Test: `internal/store/id_test.go`; modify `internal/store/types_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/store/id_test.go`:

```go
package store

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionIDFormatAndUniqueness(t *testing.T) {
	a := NewSessionID()
	b := NewSessionID()
	require.Regexp(t, uuidV4Re, a, "must be a v4 UUID")
	require.Regexp(t, uuidV4Re, b)
	require.NotEqual(t, a, b, "two ids must differ")
}
```

And extend the round-trip in `internal/store/types_test.go` — in `TestSessionJSONRoundTrip`, add the field to the literal and assert it:
- Add to the `Session{...}` literal (e.g. after `TmuxSession:`): `ClaudeSessionID: "11111111-1111-4111-8111-111111111111",`
- Add after the existing assertions: `require.Equal(t, "11111111-1111-4111-8111-111111111111", got.ClaudeSessionID)`

- [ ] **Step 2: Run to verify failure** — `go test ./internal/store/ -run 'TestNewSessionID|TestSessionJSONRoundTrip'`
Expected: FAIL (compile: `NewSessionID` undefined; `ClaudeSessionID` unknown field).

- [ ] **Step 3: Add the field.** In `internal/store/types.go`, add to the `Session` struct immediately after the `TmuxSession` field:

```go
	ClaudeSessionID string    `json:"claude_session_id"` // pinned claude --session-id (UUID); deterministic transcript + future --resume
```

- [ ] **Step 4: Add the generator.** Create `internal/store/id.go`:

```go
package store

import (
	"crypto/rand"
	"encoding/hex"
)

// NewSessionID returns a random RFC-4122 v4 UUID string, suitable for a
// `claude --session-id`. It panics only if the OS random source fails (which
// would make the daemon unable to function anyway).
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: cannot read random bytes for session id: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
```

- [ ] **Step 5: Run to verify pass** — `go test ./internal/store/ -run 'TestNewSessionID|TestSessionJSONRoundTrip'` → PASS. Then `go build ./... && go vet ./internal/store/` → clean.

- [ ] **Step 6: Commit**
```bash
git add internal/store/types.go internal/store/id.go internal/store/id_test.go internal/store/types_test.go
git commit -m "feat(store): ClaudeSessionID field + NewSessionID v4 generator"
```

---

### Task 2: `lifecycle` — pin `--session-id`/`--name` in `Spawn` (+ update existing tests)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Update the existing spawn tests first (they assert the exact launched command, which is changing).** All four reconstruct the expected command from the returned `s.ClaudeSessionID`, so they stay deterministic.

In `TestSpawnDevelopmentCreatesWorktreeTmuxAndDoc`, replace the line-81 assertion:
```go
	// Launch claude UNATTENDED, with a pinned session id and display name.
	require.NotEmpty(t, s.ClaudeSessionID)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "PROJ-350", claudeLaunch(s.ClaudeSessionID, "PROJ-350"), "Enter"})
```

In `TestSpawnNoWorktreeTypeRunsInRepoWithAutoID`, replace the line-112 assertion:
```go
	require.NotEmpty(t, s.ClaudeSessionID)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, claudeLaunch(s.ClaudeSessionID, s.ID), "Enter"})
```

In `TestSpawnPromptModeNoWorktree`, replace the `launch :=` line (currently `launch := claudeCmd + ...`):
```go
	launch := claudeLaunch(s.ClaudeSessionID, s.ID) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
```

In `TestSpawnPromptModeMultilinePromptIsFileBacked`, replace the `launch :=` line the same way:
```go
	launch := claudeLaunch(s.ClaudeSessionID, s.ID) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
	require.NotContains(t, launch, "\n", "the typed launch command must never contain a raw newline")
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/lifecycle/ -run TestSpawn`
Expected: FAIL (compile: `claudeLaunch` undefined).

- [ ] **Step 3: Add the launch builder.** In `internal/lifecycle/lifecycle.go`, just below the `claudeCmd` const (around line 37), add:

```go
// claudeLaunch builds the claude invocation for a spawned agent: the base
// command plus a pinned --session-id (deterministic transcript + future
// --resume) and a --name display label equal to the agent id, so the agent id,
// tmux session, and claude session all read the same. sessionID is a generated
// UUID (safe charset); name is the agent id (may be a ticket key) so it is quoted.
func claudeLaunch(sessionID, name string) string {
	return claudeCmd + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
}
```

- [ ] **Step 4: Generate + pin the id in `Spawn`.** In `Spawn`, immediately after the `sess := &store.Session{...}` literal is constructed (after the line setting `Status: store.StatusSpawning,` and its closing `}`), add:

```go
	sess.ClaudeSessionID = store.NewSessionID()
```

Then, in the **prompt-mode** path, change the `launch :=` line from:
```go
		launch := claudeCmd + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```
to:
```go
		launch := claudeLaunch(sess.ClaudeSessionID, id) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

And in the **typed/managed** path, change the final send-keys from:
```go
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeCmd, "Enter"); err != nil {
```
to:
```go
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeLaunch(sess.ClaudeSessionID, id), "Enter"); err != nil {
```

- [ ] **Step 5: Run to verify pass** — `go test ./internal/lifecycle/ -run TestSpawn` → PASS. Then `go build ./... && go vet ./internal/lifecycle/` → clean.

- [ ] **Step 6: Commit**
```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): pin claude --session-id and --name on spawn"
```

---

### Task 3: `lifecycle` — deterministic transcript lookup

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests.** Append to `internal/lifecycle/lifecycle_test.go`:

```go
func TestTranscriptPathBySessionIDBeatsNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-zz99"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	sid := "33333333-3333-4333-8333-333333333333"
	want := filepath.Join(dir, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("HELLO"), 0o644))
	// A decoy with a newer mtime that the legacy heuristic would wrongly pick.
	decoy := filepath.Join(dir, "decoy.jsonl")
	require.NoError(t, os.WriteFile(decoy, []byte("DECOY"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(decoy, future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-zz99", Workdir: workdir, ClaudeSessionID: sid}
	require.Equal(t, want, lc.transcriptPath(sess), "pinned id beats newest-mtime decoy")
}

func TestTranscriptPathGlobFallback(t *testing.T) {
	root := t.TempDir()
	sid := "44444444-4444-4444-8444-444444444444"
	// The transcript lives under a project dir that does NOT match the workdir
	// encoding (simulates the /tmp→/private/tmp path-resolution mismatch).
	other := filepath.Join(root, "-some-other-encoded-dir")
	require.NoError(t, os.MkdirAll(other, 0o755))
	want := filepath.Join(other, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("X"), 0o644))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-x", Workdir: "/mismatch/dir", ClaudeSessionID: sid}
	require.Equal(t, want, lc.transcriptPath(sess), "unique glob finds it despite dir mismatch")
}

func TestTranscriptPathLegacyFallsBackToNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-leg"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("OLD"), 0o644))
	newf := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(newf, []byte("NEW"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(newf, future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-leg", Workdir: workdir} // no ClaudeSessionID
	require.Equal(t, newf, lc.transcriptPath(sess), "empty id → newest .jsonl (legacy)")
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/lifecycle/ -run TestTranscriptPath`
Expected: FAIL (compile: `lc.transcriptPath` undefined).

- [ ] **Step 3: Add the resolver and refactor the newest-helper.** In `internal/lifecycle/lifecycle.go`:

Add the resolver (near `recentActivity`):
```go
// transcriptPath resolves the agent's claude transcript file. With a pinned
// ClaudeSessionID the file is exactly <id>.jsonl: look under the encoded project
// dir first, then an unambiguous glob across all project dirs (the UUID is
// globally unique, so this is robust to cwd path-encoding quirks). With no
// pinned id (legacy sessions) it falls back to the newest .jsonl in the dir.
func (l *Lifecycle) transcriptPath(sess *store.Session) string {
	if sess.ClaudeSessionID != "" {
		if dir := claudeProjectDir(l.ProjectsDir, sess.Workdir); dir != "" {
			p := filepath.Join(dir, sess.ClaudeSessionID+".jsonl")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		if l.ProjectsDir != "" {
			if m, _ := filepath.Glob(filepath.Join(l.ProjectsDir, "*", sess.ClaudeSessionID+".jsonl")); len(m) == 1 {
				return m[0]
			}
		}
		return "" // pinned but not written yet → caller falls back to the pane
	}
	if dir := claudeProjectDir(l.ProjectsDir, sess.Workdir); dir != "" {
		return newestTranscriptPath(dir)
	}
	return ""
}
```

Refactor `newestTranscriptTail` to split out the path-finding (so the legacy branch and the existing test both keep working). Replace the existing `newestTranscriptTail` function with:
```go
// newestTranscriptPath returns the path of the most recently modified *.jsonl in
// dir, or "" if none.
func newestTranscriptPath(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type f struct {
		path string
		mod  int64
	}
	var files []f
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	return files[0].path
}

// newestTranscriptTail returns up to maxBytes from the end of the most recently
// modified *.jsonl file in dir, or "" if none. (readFileTail("") is a safe "".)
func newestTranscriptTail(dir string, maxBytes int64) string {
	return readFileTail(newestTranscriptPath(dir), maxBytes)
}
```

Rewire `recentActivity` to use the resolver:
```go
func (l *Lifecycle) recentActivity(ctx context.Context, sess *store.Session) string {
	if p := l.transcriptPath(sess); p != "" {
		if txt := readFileTail(p, 4000); txt != "" {
			return txt
		}
	}
	out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", sess.TmuxSession, "-S", "-40")
	if err != nil {
		return ""
	}
	return out
}
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/lifecycle/` → PASS (new `TestTranscriptPath*`, plus the existing `TestNewestTranscriptTailPicksNewestAndTails`, `TestSummarizeUsesTranscriptThenClaudeP`, and `TestSummarizeFallsBackToPane` still green — they exercise the empty-id legacy path). Then `go build ./... && go vet ./internal/lifecycle/` → clean.

- [ ] **Step 5: Commit**
```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): resolve transcript by pinned session id (glob fallback; legacy newest)"
```

---

### Task 4: surface `ClaudeSessionID` in `agentctl status`

**Files:**
- Modify: `internal/cli/sessions.go`

- [ ] **Step 1: Add the `claude:` line to the status render.** In `newStatusCmd`, replace the existing `fmt.Fprintf(out, "id: …", …)` block (the multi-field status print) with one that adds a `claude:` line:

```go
			fmt.Fprintf(out, "id:       %s\ntype:     %s\nticket:   %s\nstatus:   %s\nrepo:     %s\nworkdir:  %s\nworktree: %s\nbranch:   %s\npr:       %s\nsubject:  %s\nclaude:   %s\nupdated:  %s\n",
				s.ID, typeOrPending(s.Type), s.Ticket, s.Status, s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, s.Subject, s.ClaudeSessionID, s.UpdatedAt.Format(time.RFC3339))
```

(Only the format string gains `\nclaude:   %s` before `\nupdated:`, and `s.ClaudeSessionID` is inserted before `s.UpdatedAt`. No other lines change. The daemon API / client / MCP / GUI already serialize the field — no change needed there.)

- [ ] **Step 2: Build + vet** — `go build ./... && go vet ./internal/cli/` → clean. (The `status` renderer has no unit test today; the field is plumbed identically to `subject`/`pr`, so the build is the check. Behavior is covered end-to-end by the store round-trip test in Task 1.)

- [ ] **Step 3: Commit**
```bash
git add internal/cli/sessions.go
git commit -m "feat(cli): show claude session id in agentctl status"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green, no Docker.
- [ ] Optional live smoke: `make build`, start the daemon against a temp data dir, `agentctl start "say hi"`, then `agentctl status <id>` shows a `claude:` UUID line and `~/.claude/projects/*/<that-uuid>.jsonl` exists.

Then proceed to **superpowers:finishing-a-development-branch**.
