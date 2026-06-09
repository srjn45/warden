# agentctl Richer Context (workdir + auto-subject) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface, per agent, its working directory and an auto-generated one-line "subject" of what it's doing — with prompt-spawned agents each getting their own dir (`~/agentctl-agents/<id>/`), the subject summarized by Claude from the agent's transcript (fallback: pane), seeded at spawn and refreshed on a throttled cadence by the poller, shown in `ls`/`status`/GUI and over MCP.

**Architecture:** Additive. `store.Session` gains `workdir` + `subject`. `lifecycle.Spawn` records the workdir (per-agent subdir for prompt mode) and seeds the subject from the prompt. A `lifecycle.Summarize` reads the agent's Claude transcript (by encoded workdir, else the tmux pane) and runs `claude -p` to produce a short subject. The poller refreshes the subject on a throttled cadence (only when the pane changed and at most every `summarizeAfter`), pushing updates over the existing SSE/OnChange path.

**Tech Stack:** Go 1.26 (mockable Runner, `go:embed` already in tree), MongoDB, Astro/React (display only). Claude integration via `claude -p` + transcript files under `~/.claude/projects`.

**Reference spec:** `docs/superpowers/specs/2026-06-02-agentctl-context-design.md`

---

## Conventions
- Module `github.com/srajanpathak/agentctl`. Executor sets up an isolated worktree first.
- Go: strict TDD. Frontend pure logic: Vitest; components: build/tsc verify.
- Commit after each task with the given message (no Co-Authored-By footer).
- Go tests `go test ./...` (store/daemon need Docker mongo → `make mongo-up`). Frontend `cd web && npm test`.

## File map
```
internal/store/types.go          + Workdir, Subject on Session
internal/store/store.go          + UpdateSubject in interface
internal/store/mongo.go          + MongoStore.UpdateSubject
internal/store/{types,mongo}_test.go ~ persist + UpdateSubject
internal/daemon/api_test.go      + fakeStore.UpdateSubject (interface compliance)
internal/config/config.go        + ClaudeProjectsDir + default
internal/config/config_test.go   + tests
internal/lifecycle/lifecycle.go  + ProjectsDir field, per-agent workdir+seed in Spawn,
                                   set sess.Workdir (both modes), Summarize + helpers
internal/lifecycle/lifecycle_test.go + firstWords/parseSummary/claudeProjectDir/recentActivity/Summarize/Spawn-workdir tests
internal/poller/poller.go        + Deps Summarize/UpdateSubject, Poller SummarizeAfter+lastSummary, throttled refresh in tick
internal/poller/poller_test.go   ~ stubDeps +Summarize/UpdateSubject; + refresh tests
internal/daemon/poller_deps.go   ~ NewPollerDeps(st,run,lc); + Summarize/UpdateSubject
internal/cli/daemon.go           ~ NewPollerDeps(st,runner,lc); set lc.ProjectsDir
internal/cli/sessions.go         ~ ls DIR+SUBJECT columns; status workdir+subject
internal/client/... (none)
web/src/lib/types.ts             + workdir, subject
web/src/components/AgentList.tsx ~ subject column
web/src/components/AgentDetail.tsx ~ workdir + subject
README.md                        ~ note (optional, Phase 7)
```

Phase order: **1** store → **2** config → **3** lifecycle → **4** poller+daemon wiring → **5** cli → **6** web → **7** integration.

---

## Phase 1 — Store: Workdir + Subject + UpdateSubject

### Task 1.1: Add fields + UpdateSubject

**Files:** `internal/store/types.go`, `store.go`, `mongo.go`, `types_test.go`, `mongo_test.go`, `internal/daemon/api_test.go`

- [ ] **Step 1: Failing tests**

In `internal/store/types_test.go`, extend the `Session{...}` literal in `TestSessionBSONRoundTrip` (after `Prompt`):
```go
		Workdir: "/Users/me/agentctl-agents/agent-a1b2",
		Subject: "review auth module for security",
```
and after the existing asserts:
```go
	require.Equal(t, "/Users/me/agentctl-agents/agent-a1b2", got.Workdir)
	require.Equal(t, "review auth module for security", got.Subject)
```

In `internal/store/mongo_test.go`, append:
```go
func TestUpdateSubject(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.UpdateSubject(ctx, "PROJ-350", "investigating flaky test"))
	got, _ := st.Get(ctx, "PROJ-350")
	require.Equal(t, "investigating flaky test", got.Subject)
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/store/ -run 'TestSessionBSONRoundTrip|TestUpdateSubject' 2>&1 | head`
Expected: compile error / FAIL — no `Workdir`/`Subject`/`UpdateSubject`.

- [ ] **Step 3: Add fields**

In `internal/store/types.go`, add to `Session` (after `Prompt`):
```go
	Workdir         string    `bson:"workdir" json:"workdir"`         // absolute cwd of the tmux session
	Subject         string    `bson:"subject" json:"subject"`         // one-line auto summary of what it's doing
```

- [ ] **Step 4: Add to interface + MongoStore**

In `internal/store/store.go`, add to the `Store` interface (after `UpdateType`):
```go
	UpdateSubject(ctx context.Context, id, subject string) error
```
In `internal/store/mongo.go`, add (next to `UpdateType`):
```go
func (m *MongoStore) UpdateSubject(ctx context.Context, id, subject string) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"subject": subject})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 5: fakeStore compliance**

In `internal/daemon/api_test.go`, add to `fakeStore` (next to `UpdateType`):
```go
func (f *fakeStore) UpdateSubject(_ context.Context, id, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Subject = subject
	return nil
}
```
(If `fakeStore` already has a `mu sync.Mutex` from the prompt-spawn race fix, reuse it as shown; if not, drop the `f.mu` lines.)

- [ ] **Step 6: Run to verify pass**

Run: `make mongo-up >/dev/null 2>&1; go build ./... && go test ./internal/store/ ./internal/daemon/`
Expected: PASS (incl. `TestUpdateSubject`).

- [ ] **Step 7: Commit**

```bash
git add internal/store internal/daemon/api_test.go
git commit -m "feat: store Session.Workdir/Subject + UpdateSubject"
```

---

## Phase 2 — Config: ClaudeProjectsDir

### Task 2.1: Add ClaudeProjectsDir

**Files:** `internal/config/config.go`, `internal/config/config_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/config/config_test.go`:
```go
func TestClaudeProjectsDirDefault(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.ClaudeProjectsDir, ".claude/projects"), "got %q", c.ClaudeProjectsDir)
}

func TestClaudeProjectsDirFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_PROJECTS_DIR", "/tmp/projects")
	require.Equal(t, "/tmp/projects", Load().ClaudeProjectsDir)
}
```
(`strings` is already imported from the prior workdir tests.)

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/config/`
Expected: FAIL — no `ClaudeProjectsDir`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the field to `Config`:
```go
	ClaudeProjectsDir string
```
add a helper:
```go
func defaultClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}
```
and in `Load`'s returned struct:
```go
		ClaudeProjectsDir: envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
```

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/config/`
Expected: PASS.
```bash
git add internal/config
git commit -m "feat: config CLAUDE_PROJECTS_DIR (default ~/.claude/projects)"
```

---

## Phase 3 — Lifecycle: per-agent workdir, seed subject, Summarize

### Task 3.1: Pure helpers — firstWords, parseSummary, claudeProjectDir, summaryArg

**Files:** `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestFirstWords(t *testing.T) {
	require.Equal(t, "review the auth module", firstWords("review the auth module", 10))
	require.Equal(t, "one two three…", firstWords("one two three four five", 3))
	require.Equal(t, "", firstWords("", 5))
}

func TestParseSummary(t *testing.T) {
	require.Equal(t, "review auth module for security", parseSummary("review auth module for security\n"))
	require.Equal(t, "tracing a flaky test", parseSummary(`"tracing a flaky test"`))
	require.Equal(t, "first line only", parseSummary("first line only\nsecond ignored"))
}

func TestClaudeProjectDir(t *testing.T) {
	got := claudeProjectDir("/root/projects", "/Users/srajan.pathak/agentctl-agents/agent-a1b2")
	require.Equal(t, "/root/projects/-Users-srajan-pathak-agentctl-agents-agent-a1b2", got)
	require.Equal(t, "", claudeProjectDir("", "/anything")) // empty root → no transcript lookup
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/lifecycle/ -run 'TestFirstWords|TestParseSummary|TestClaudeProjectDir'`
Expected: FAIL — undefined helpers.

- [ ] **Step 3: Implement the helpers**

In `internal/lifecycle/lifecycle.go`, add `"regexp"` and `"path/filepath"` (filepath is already imported) to imports, then:
```go
const summaryInstruction = "In 8 words or fewer, summarize what this agent is currently working on. Reply with ONLY the phrase — no quotes, no preamble.\n\nRecent activity:\n"

// firstWords returns the first n whitespace-separated words of s, appending an
// ellipsis when truncated. Used to seed a subject from the prompt (no LLM call).
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) <= n {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:n], " ") + "…"
}

// parseSummary cleans an LLM summary reply into a single short line.
func parseSummary(out string) string {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "\"'`")
	line = strings.Join(strings.Fields(line), " ")
	if len(line) > 80 {
		line = strings.TrimSpace(line[:80])
	}
	return line
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeProjectDir maps an absolute workdir to its Claude Code transcript
// project directory under root (Claude replaces non-alphanumerics with '-').
// Returns "" when root is empty (transcript lookup disabled).
func claudeProjectDir(root, workdir string) string {
	if root == "" || workdir == "" {
		return ""
	}
	return filepath.Join(root, nonAlnum.ReplaceAllString(workdir, "-"))
}

func summaryArg(text string) string {
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	return summaryInstruction + text
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/lifecycle/ -run 'TestFirstWords|TestParseSummary|TestClaudeProjectDir'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: lifecycle summary helpers (firstWords/parseSummary/claudeProjectDir/summaryArg)"
```

### Task 3.2: Per-agent workdir + seeded subject in Spawn

**Files:** `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestSpawnPromptModePerAgentWorkdir(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "research SSE reconnection semantics"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Prompt: prompt, Workdir: "/home/me/agentctl-agents"})
	require.NoError(t, err)
	expDir := "/home/me/agentctl-agents/" + s.ID
	require.Equal(t, expDir, s.Workdir, "per-agent subdir")
	require.Equal(t, prompt, firstWordsExpand(s.Subject, prompt), "subject seeded from prompt")
	// Creates the dir and starts tmux there.
	require.Contains(t, fr.calledArgs(), []string{"mkdir", "-p", expDir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", expDir})
}

// firstWordsExpand is a test helper: the seeded subject is firstWords(prompt,10);
// for a <=10-word prompt that equals the prompt, so this just returns prompt when they match.
func firstWordsExpand(subject, prompt string) string {
	if subject == firstWords(prompt, 10) {
		return prompt
	}
	return subject
}

func TestSpawnTypedModeRecordsWorkdir(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git worktree list --porcelain": {Out: noOtherWorktrees},
	}}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeDevelopment, Ticket: "A-1", Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "/repo/.worktrees/A-1", s.Workdir, "typed worktree dir recorded")
}

func TestSpawnNoWorktreeTypeRecordsRepoWorkdir(t *testing.T) {
	fr := &FakeRunner{}
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{Type: store.TypeBuildkiteDebug, Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "/repo", s.Workdir, "no-worktree type runs in repo")
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/lifecycle/ -run 'TestSpawnPromptModePerAgentWorkdir|TestSpawnTypedModeRecordsWorkdir|TestSpawnNoWorktreeTypeRecordsRepoWorkdir'`
Expected: FAIL — prompt mode uses `req.Workdir` (no per-agent dir, no `mkdir`), `sess.Workdir`/`Subject` unset.

- [ ] **Step 3: Update Spawn**

In `internal/lifecycle/lifecycle.go`, set the seeded subject in the `sess` literal (add after `Prompt: req.Prompt,`):
```go
		Subject:     firstWords(req.Prompt, 10),
```
Replace the **prompt-mode** block with the per-agent-dir version:
```go
	if promptMode {
		dir := filepath.Join(req.Workdir, id)
		if out, err := l.run.Run(ctx, "", "mkdir", "-p", dir); err != nil {
			return nil, fmt.Errorf("mkdir workdir: %w: %s", err, out)
		}
		sess.Workdir = dir
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", dir); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
		launch := claudeCmd + " " + shellQuoteArg(req.Prompt)
		if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
			return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
		}
		return sess, nil
	}
```
In the **typed** path, record the workdir — add `sess.Workdir = workdir` right after the `workdir` variable is finalized (after the `if wantWorktree(req) { ... }` block, before the `tmux new-session` call):
```go
	sess.Workdir = workdir
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/lifecycle/`
Expected: PASS — the 3 new tests plus all existing Spawn/Cleanup tests. NOTE: the existing `TestSpawnPromptModeNoWorktree` from the prompt-spawn feature asserted `tmux new-session ... -c /home/me/agentctl-agents` (the base dir) and no git calls. The per-agent-dir change makes the workdir `/home/me/agentctl-agents/<id>` and adds a `mkdir -p` call. **Update that existing test**: change its `-c` expectation to `"/home/me/agentctl-agents/"+s.ID`, keep the "no git calls" assertion (mkdir/tmux are not git), and (if it asserted `s.Workdir`/empty) align to the per-agent dir. Make this edit as part of Step 3 so the suite is green.

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: per-agent workdir + seeded subject in Spawn; record workdir (typed)"
```

### Task 3.3: Summarize (transcript → pane → claude -p)

**Files:** `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/lifecycle/lifecycle_test.go` (add `"os"`, `"path/filepath"` to the test imports if not present):
```go
func TestSummarizeUsesTranscriptThenClaudeP(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/agentctl-agents/agent-zz99"
	projDir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(projDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projDir, "sess.jsonl"),
		[]byte(`{"role":"user","text":"look into the auth bug"}`+"\n"), 0o644))

	fr := &FakeRunner{Responses: map[string]FakeResp{}}
	lc := New(fr)
	lc.ProjectsDir = root
	// The claude -p call is keyed by the transcript-derived text.
	sess := &store.Session{ID: "agent-zz99", TmuxSession: "agent-zz99", Workdir: workdir}
	// Stub claude -p for whatever arg is built from the transcript text:
	fr.Responses["claude -p "+summaryArg(`{"role":"user","text":"look into the auth bug"}`+"\n")] = FakeResp{Out: "looking into the auth bug\n"}

	got, err := lc.Summarize(context.Background(), sess)
	require.NoError(t, err)
	require.Equal(t, "looking into the auth bug", got)
}

func TestSummarizeFallsBackToPane(t *testing.T) {
	root := t.TempDir() // empty → no transcript
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux capture-pane -p -t agent-aa11 -S -40": {Out: "building the REST handler\n"},
		"claude -p " + summaryArg("building the REST handler\n"): {Out: "building a REST handler"},
	}}
	lc := New(fr)
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-aa11", TmuxSession: "agent-aa11", Workdir: "/Users/me/agentctl-agents/agent-aa11"}
	got, err := lc.Summarize(context.Background(), sess)
	require.NoError(t, err)
	require.Equal(t, "building a REST handler", got)
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/lifecycle/ -run TestSummarize`
Expected: FAIL — `lc.ProjectsDir`/`Summarize`/`recentActivity` undefined.

- [ ] **Step 3: Implement Summarize + recentActivity + ProjectsDir field**

In `internal/lifecycle/lifecycle.go`, add `"os"` and `"sort"` to imports. Add a `ProjectsDir` field to `Lifecycle`:
```go
type Lifecycle struct {
	run Runner
	// ProjectsDir is the Claude Code transcript root (default empty → transcript
	// lookup disabled; the daemon sets it from config). Overridable in tests.
	ProjectsDir string
}
```
Add the methods:
```go
// Summarize produces a one-line subject for an agent: it reads recent activity
// (transcript, else pane) and asks claude -p for an <=8-word phrase.
func (l *Lifecycle) Summarize(ctx context.Context, sess *store.Session) (string, error) {
	text := l.recentActivity(ctx, sess)
	if strings.TrimSpace(text) == "" {
		text = sess.Prompt // last resort: the original prompt
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	out, err := l.run.Run(ctx, "", "claude", "-p", summaryArg(text))
	if err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseSummary(out), nil
}

// recentActivity returns recent conversation text: the tail of the newest
// transcript .jsonl under the agent's project dir, else the tmux pane.
func (l *Lifecycle) recentActivity(ctx context.Context, sess *store.Session) string {
	if dir := claudeProjectDir(l.ProjectsDir, sess.Workdir); dir != "" {
		if txt := newestTranscriptTail(dir, 4000); txt != "" {
			return txt
		}
	}
	out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", sess.TmuxSession, "-S", "-40")
	if err != nil {
		return ""
	}
	return out
}

// newestTranscriptTail returns up to maxBytes from the end of the most recently
// modified *.jsonl file in dir, or "" if none.
func newestTranscriptTail(dir string, maxBytes int64) string {
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
	data, err := os.ReadFile(files[0].path)
	if err != nil {
		return ""
	}
	if int64(len(data)) > maxBytes {
		data = data[int64(len(data))-maxBytes:]
	}
	return string(data)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/lifecycle/`
Expected: PASS (both Summarize tests + all prior).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: lifecycle Summarize (transcript→pane→claude -p) + ProjectsDir"
```

---

## Phase 4 — Poller refresh + daemon wiring

### Task 4.1: Poller Deps + throttled subject refresh

**Files:** `internal/poller/poller.go`, `internal/poller/poller_test.go`

- [ ] **Step 1: Failing tests**

In `internal/poller/poller_test.go`, first extend `stubDeps` with the two new methods + recording fields. Add fields to the struct:
```go
	subjects     map[string]string // records UpdateSubject calls
	summary      string            // canned Summarize result
	summarizeErr error
	summarizeN   int               // count of Summarize calls
```
and methods:
```go
func (d *stubDeps) UpdateSubject(_ context.Context, id, subject string) error {
	if d.subjects == nil {
		d.subjects = map[string]string{}
	}
	d.subjects[id] = subject
	return nil
}
func (d *stubDeps) Summarize(_ context.Context, s *store.Session) (string, error) {
	d.summarizeN++
	return d.summary, d.summarizeErr
}
```
Append tests:
```go
func TestTickRefreshesSubjectWhenPaneChangedAndDue(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "old"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "new pane text"}, // changed
		updates:  map[string]store.Status{},
		summary:  "doing the thing",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = 0 // always due
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, d.summarizeN)
	require.Equal(t, "doing the thing", d.subjects["A-1"])
}

func TestTickSkipsSummaryWhenPaneUnchanged(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "same"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "same"}, // unchanged
		updates:  map[string]store.Status{},
		summary:  "x",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = 0
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 0, d.summarizeN, "no summary when pane didn't change")
}

func TestTickThrottlesSummary(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "old"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "changed"},
		updates:  map[string]store.Status{},
		summary:  "y",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = time.Hour // not due (lastSummary set on first call)
	require.NoError(t, p.tick(context.Background()))           // first: due (zero time) → summarizes
	d.panes["A-1"] = "changed again"
	require.NoError(t, p.tick(context.Background()))           // second: within the hour → throttled
	require.Equal(t, 1, d.summarizeN, "throttled to one within the interval")
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/poller/ -run 'TestTickRefreshes|TestTickSkipsSummary|TestTickThrottles'`
Expected: FAIL — `Deps` lacks `Summarize`/`UpdateSubject`; `p.SummarizeAfter` undefined.

- [ ] **Step 3: Extend Deps + Poller + tick**

In `internal/poller/poller.go`, add to the `Deps` interface:
```go
	Summarize(ctx context.Context, s *store.Session) (string, error)
	UpdateSubject(ctx context.Context, id, subject string) error
```
Extend `Poller` + `New`:
```go
type Poller struct {
	deps           Deps
	stuckAfter     time.Duration
	SummarizeAfter time.Duration       // throttle for subject refresh (0 = every change)
	lastSummary    map[string]time.Time
	OnChange       func()
}

func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:           d,
		stuckAfter:     stuckAfter,
		SummarizeAfter: 2 * time.Minute,
		lastSummary:    map[string]time.Time{},
	}
}
```
In `tick`, track per-session pane change and add the throttled refresh. Replace the loop body so it reads:
```go
	now := time.Now()
	changed := false
	for _, s := range sessions {
		if isTerminal(s.Status) {
			continue
		}
		alive := p.deps.SessionAlive(ctx, s.TmuxSession)
		var pane string
		paneChanged := false
		if alive {
			pane, _ = p.deps.CapturePane(ctx, s.TmuxSession)
			if excerpt := lastLines(pane, 20); excerpt != s.LastPaneExcerpt {
				_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
				changed = true
				paneChanged = true
			}
		}
		next := classify(s, pane, alive, time.Since(s.UpdatedAt), p.stuckAfter)
		if next != s.Status {
			if err := p.deps.UpdateStatus(ctx, s.ID, next); err != nil {
				log.Printf("poller: update %s: %v", s.ID, err)
			} else {
				changed = true
			}
		}
		if alive && paneChanged && now.Sub(p.lastSummary[s.ID]) >= p.SummarizeAfter {
			if subj, err := p.deps.Summarize(ctx, s); err != nil {
				log.Printf("poller: summarize %s: %v", s.ID, err)
			} else if subj != "" && subj != s.Subject {
				if err := p.deps.UpdateSubject(ctx, s.ID, subj); err != nil {
					log.Printf("poller: subject %s: %v", s.ID, err)
				} else {
					changed = true
				}
			}
			p.lastSummary[s.ID] = now
		}
	}
	if changed && p.OnChange != nil {
		p.OnChange()
	}
	return nil
```
(Remove the old `now`/`changed`/loop lines this replaces — there must be exactly one `now := time.Now()` and one loop.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/poller/`
Expected: PASS — the 3 new tests + all existing poller tests (existing tick tests: `TestTickUpdatesPaneOnlyWhenChanged`'s "diff" session now also calls `stubDeps.Summarize` which returns `d.summary==""` → no `UpdateSubject`, harmless; confirm those still pass).

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat: poller throttled subject refresh (Summarize/UpdateSubject)"
```

### Task 4.2: Daemon pollerDeps + wiring

**Files:** `internal/daemon/poller_deps.go`, `internal/cli/daemon.go`

- [ ] **Step 1: Implement pollerDeps Summarize/UpdateSubject (+ take lifecycle)**

Replace `internal/daemon/poller_deps.go`:
```go
package daemon

import (
	"context"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/poller"
	"github.com/srajanpathak/agentctl/internal/store"
)

type pollerDeps struct {
	store store.Store
	run   lifecycle.Runner
	lc    *lifecycle.Lifecycle
}

// NewPollerDeps adapts the store + Runner + lifecycle to the poller's Deps.
func NewPollerDeps(st store.Store, run lifecycle.Runner, lc *lifecycle.Lifecycle) poller.Deps {
	return &pollerDeps{store: st, run: run, lc: lc}
}

func (d *pollerDeps) List(ctx context.Context) ([]*store.Session, error) { return d.store.List(ctx) }
func (d *pollerDeps) UpdateStatus(ctx context.Context, id string, st store.Status) error {
	return d.store.UpdateStatus(ctx, id, st)
}
func (d *pollerDeps) UpdatePane(ctx context.Context, id, ex string) error {
	return d.store.UpdatePane(ctx, id, ex)
}
func (d *pollerDeps) UpdateSubject(ctx context.Context, id, subject string) error {
	return d.store.UpdateSubject(ctx, id, subject)
}
func (d *pollerDeps) Summarize(ctx context.Context, s *store.Session) (string, error) {
	return d.lc.Summarize(ctx, s)
}
func (d *pollerDeps) SessionAlive(ctx context.Context, name string) bool {
	_, err := d.run.Run(ctx, "", "tmux", "has-session", "-t", name)
	return err == nil
}
func (d *pollerDeps) CapturePane(ctx context.Context, name string) (string, error) {
	return d.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", name)
}
```

- [ ] **Step 2: Wire in the daemon command**

In `internal/cli/daemon.go`, set the lifecycle's ProjectsDir and pass `lc` to `NewPollerDeps`:
```go
			runner := lifecycle.ExecRunner{}
			lc := lifecycle.New(runner)
			lc.ProjectsDir = cfg.ClaudeProjectsDir
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner, lc)
			pl := poller.New(pd, 5*time.Minute)
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.Workdir)
```

- [ ] **Step 3: Build + tests**

Run: `go build ./... && go vet ./... && go test ./internal/daemon/ ./internal/poller/`
Expected: clean / PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/poller_deps.go internal/cli/daemon.go
git commit -m "feat: wire lifecycle summarizer + ProjectsDir into the poller"
```

---

## Phase 5 — CLI display

### Task 5.1: ls DIR+SUBJECT + status workdir/subject

**Files:** `internal/cli/sessions.go`

- [ ] **Step 1: Update ls**

In `internal/cli/sessions.go`, in `newLsCmd`'s `RunE`, replace the header + row block:
```go
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tAGE\tDIR\tSUBJECT")
			for _, s := range sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, typeOrPending(s.Type), s.Status, age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
			}
			return tw.Flush()
```
Add helpers at the bottom of the file (and `"path/filepath"` to imports):
```go
func dirName(workdir string) string {
	if workdir == "" {
		return "—"
	}
	return filepath.Base(workdir)
}

func typeOrPending(t store.Type) string {
	if t == "" {
		return "…"
	}
	return string(t)
}
```
Remove the now-unused `lastDetail` helper if nothing else references it (check with `grep -n lastDetail internal/cli/sessions.go`; if only the old row used it, delete it).

- [ ] **Step 2: Update status**

In `newStatusCmd`'s `RunE`, replace the `fmt.Fprintf(out, "id: ...")` block to include workdir + subject:
```go
			fmt.Fprintf(out, "id:       %s\ntype:     %s\nticket:   %s\nstatus:   %s\nrepo:     %s\nworkdir:  %s\nworktree: %s\nbranch:   %s\npr:       %s\nsubject:  %s\nupdated:  %s\n",
				s.ID, typeOrPending(s.Type), s.Ticket, s.Status, s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, s.Subject, s.UpdatedAt.Format(time.RFC3339))
```

- [ ] **Step 3: Build + manual check**

Run: `go build ./... && go vet ./...`
Expected: clean. (`ls`/`status` exercised live in Phase 7.)

- [ ] **Step 4: Commit**

```bash
git add internal/cli/sessions.go
git commit -m "feat: ls shows DIR+SUBJECT; status shows workdir+subject"
```

---

## Phase 6 — Web display

### Task 6.1: types + AgentList + AgentDetail

**Files:** `web/src/lib/types.ts`, `web/src/components/AgentList.tsx`, `web/src/components/AgentDetail.tsx`

- [ ] **Step 1: types.ts**

In `web/src/lib/types.ts`, add to `Session` (after `prompt`):
```ts
  workdir: string;
  subject: string;
```

- [ ] **Step 2: AgentList — subject column**

In `web/src/components/AgentList.tsx`, add a Subject header `<th>Subject</th>` after `Detail` (or replace the Detail column). Replace the `lastDetail` cell with the subject; final row:
```tsx
            <tr key={s.id} className={s.id === selectedId ? 'sel' : ''} onClick={() => onSelect(s.id)}>
              <td>{s.id}</td>
              <td>{s.type || <span className="muted">classifying…</span>}</td>
              <td><BusyIdleBadge status={s.status} /></td>
              <td>{s.status}</td>
              <td>{age(s.updated_at)}</td>
              <td className="muted">{s.subject}</td>
            </tr>
```
Update the `<thead>` to `<tr><th>ID</th><th>Type</th><th>State</th><th>Status</th><th>Age</th><th>Subject</th></tr>` and remove the now-unused `lastDetail` function.

- [ ] **Step 3: AgentDetail — workdir + subject**

In `web/src/components/AgentDetail.tsx`, update the metadata `<code>` line to include workdir + subject:
```tsx
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || '—'}
          {session.subject && ` · ${session.subject}`}
        </code>
```

- [ ] **Step 4: Typecheck + build + tests**

Run: `cd web && npx tsc --noEmit && npm run build && npm test`
Expected: clean / green (no api change → existing vitest suites still pass).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/types.ts web/src/components/AgentList.tsx web/src/components/AgentDetail.tsx
git commit -m "feat(web): show workdir + subject in list and detail"
```

---

## Phase 7 — Integration + README

### Task 7.1: Full build + suites + README note

**Files:** `README.md`

- [ ] **Step 1: Full verification**

Run:
```bash
make release && make mongo-up
go build ./... && go vet ./... && go test ./...
go test -race ./internal/daemon/ ./internal/poller/
cd web && npm test && cd ..
```
Expected: all green.

- [ ] **Step 2: Manual smoke (verifies per-agent dir + subject)**

> Run on an alternate port if `:8765` is busy; requires `claude` + `tmux` for real summaries.
```bash
AGENTCTL_ADDR=127.0.0.1:8799 ./bin/agentctl daemon & sleep 1
curl -s -X POST localhost:8799/spawn -H 'Content-Type: application/json' -d '{"prompt":"research SSE reconnection"}'
ls -d ~/agentctl-agents/agent-* 2>/dev/null    # per-agent dir created
AGENTCTL_ADDR=127.0.0.1:8799 ./bin/agentctl ls # DIR + SUBJECT columns (subject seeded from prompt, then refreshed)
sleep 130 && AGENTCTL_ADDR=127.0.0.1:8799 ./bin/agentctl ls  # subject refreshed by the poller (claude -p)
kill %1; tmux kill-server 2>/dev/null
```
Expected: a `~/agentctl-agents/agent-<id>/` dir exists; `ls` shows DIR=`agent-<id>` and a SUBJECT; after the refresh window the subject reflects current activity. (Sandbox blocking localhost → rely on the suites.)

- [ ] **Step 3: README note**

In `README.md`, add to the relevant sections: each agent now shows its **working directory** and an **auto-generated subject** (in `ls`/`status`, the GUI, and MCP); prompt-spawned agents get their own dir `~/agentctl-agents/<id>/`; the subject is summarized by Claude from the agent's transcript (fallback: pane), seeded from the prompt and refreshed periodically; `CLAUDE_PROJECTS_DIR` (default `~/.claude/projects`) configures transcript lookup.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: per-agent workdir + auto-subject"
```

---

## Self-review against the spec

**Spec coverage** (`2026-06-02-agentctl-context-design.md`):
- §2/§3 per-agent workdir (`~/agentctl-agents/<id>/`) + `mkdir` + `sess.Workdir` (both modes) — Task 3.2. ✅
- §2 `Session.workdir`/`subject` + `UpdateSubject` — Task 1.1. ✅
- §2/§4.1 hybrid summarizer (transcript via encoded workdir → pane → `claude -p`) — Tasks 3.1 (`claudeProjectDir`/`summaryArg`/`parseSummary`) + 3.3 (`Summarize`/`recentActivity`/`newestTranscriptTail`). ✅
- §4.2 seed subject from prompt at spawn (`firstWords`) — Tasks 3.1 + 3.2. ✅
- §4.3 throttled poller refresh (pane-changed AND interval) → SSE — Task 4.1 + 4.2 wiring. ✅
- §2 `CLAUDE_PROJECTS_DIR` config — Task 2.1; wired via `lc.ProjectsDir` — Task 4.2. ✅
- §5 display: `ls` DIR+SUBJECT, `status` workdir+subject (Task 5.1); GUI workdir+subject (Task 6.1); MCP auto via Session JSON (no change). ✅
- §6 error handling: transcript→pane→seed fallback (3.3); `Summarize` error logged/skipped in tick (4.1); `mkdir` failure aborts spawn (3.2). ✅
- §7 testing: store (1.1), config (2.1), lifecycle helpers + Summarize + Spawn-workdir (3.x), poller throttle (4.1), cli/web (5/6). ✅

**Placeholder scan:** None. Every code step has complete code; run steps state expected output. Task 3.2 Step 4 explicitly calls out updating the pre-existing `TestSpawnPromptModeNoWorktree` to the per-agent-dir expectation (with exact change) rather than hand-waving.

**Type consistency:** `store.Session.Workdir/Subject` + `store.UpdateSubject(ctx,id,subject)` used identically in `MongoStore`, `fakeStore`, `pollerDeps`, and `cli`. `lifecycle.Summarize(ctx,*store.Session)→(string,error)` matches `poller.Deps.Summarize` and `pollerDeps.Summarize` (delegates to `lc`). `Lifecycle.ProjectsDir` set by `cli/daemon.go` from `cfg.ClaudeProjectsDir`; defaulted empty in `New` (transcript lookup disabled when unset). `poller.New` signature is UNCHANGED (`SummarizeAfter` is an exported field defaulted in `New`), so existing `New(d, stuckAfter)` call sites (cli + tests) keep compiling; `NewPollerDeps` gains a third arg (`lc`) with its single non-test call site updated in `cli/daemon.go`. `claudeProjectDir(root,workdir)` encoding (`[^A-Za-z0-9]→-`) matches Claude Code's observed scheme (`/Users/srajan.pathak/…` → `-Users-srajan-pathak-…`). `web` `Session.workdir/subject` mirror the Go JSON tags.
