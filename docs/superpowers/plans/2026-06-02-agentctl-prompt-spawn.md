# agentctl Prompt-Spawn + Auto-Type Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an agent be created from a single initial prompt (no repo/type/fields), run it in a shared workdir with no assumed git worktree, deliver the prompt to Claude, and auto-assign its type label by classifying the prompt with the same Claude (headless `claude -p`), asynchronously, pushed live over SSE.

**Architecture:** Additive to the existing daemon. `POST /spawn` accepts EITHER `{prompt}` (new prompt mode) OR `{type, repo, …}` (existing typed/worktree mode). Prompt mode spawns a plain `claude --dangerously-skip-permissions '<prompt>'` in `AGENTCTL_WORKDIR` (default `~/agentctl-agents`), inserts the doc with empty type, returns 201, then a background goroutine runs `claude -p` to classify the prompt and updates the type (SSE push). The typed/worktree flow is unchanged.

**Tech Stack:** Go 1.26 (chi, exec via mockable Runner), MongoDB (mongo-driver), Astro 5 + React 19, Vitest.

**Reference spec:** `docs/superpowers/specs/2026-06-02-agentctl-prompt-spawn-design.md`

---

## Conventions
- Module path: `github.com/srajanpathak/agentctl`. Executor sets up an isolated worktree first.
- Go: strict TDD. Frontend pure logic (api.ts): TDD with Vitest; React components: build/tsc verify.
- Commit after each task with the given message (no Co-Authored-By footer).
- Go tests: `go test ./...` (store/daemon need Docker mongo → `make mongo-up`). Frontend: `cd web && npm test`.

## File map (created/modified)
```
internal/store/types.go          + Prompt field on Session
internal/store/store.go          + UpdateType in the Store interface
internal/store/mongo.go          + MongoStore.UpdateType
internal/store/types_test.go     ~ Prompt round-trip
internal/store/mongo_test.go     + TestUpdateType
internal/config/config.go        + Workdir + defaultWorkdir()
internal/config/config_test.go   + Workdir default/env tests
internal/lifecycle/lifecycle.go  + Prompt/Workdir on SpawnRequest, prompt-mode Spawn,
                                   shellQuoteArg, classifyArg/parseType/Classify, resolveID "agent" prefix
internal/lifecycle/lifecycle_test.go + prompt-mode + classify + shellquote tests
internal/daemon/api.go           ~ SpawnRequest DTO (+Prompt/Workdir), Lifecycle iface (+Classify),
                                   Server (+workdir field)
internal/daemon/server.go        ~ NewServer(...,workdir)
internal/daemon/lifecycle_routes.go ~ handleSpawn (prompt|typed), classifyAndUpdate goroutine
internal/daemon/lifecycle_adapter.go ~ adapter.Spawn maps Prompt/Workdir; + adapter.Classify
internal/daemon/api_test.go      ~ fakeStore + UpdateType (interface compliance)
internal/daemon/lifecycle_routes_test.go ~ fakeLife + Classify; + prompt-spawn tests
internal/cli/daemon.go           ~ MkdirAll(workdir) + NewServer(...,cfg.Workdir)
internal/cli/lifecycle.go        ~ start "<prompt>" path (no --type)
internal/client/client.go        ~ SpawnParams +Prompt; spawn body +prompt
web/src/lib/types.ts             + prompt on Session
web/src/lib/api.ts               ~ SpawnParams optional fields +prompt; spawn body +prompt
web/src/lib/api.test.ts          ~ spawn body includes prompt; + prompt-only test
web/src/components/NewAgentModal.tsx ~ single prompt textarea
web/src/components/AgentList.tsx ~ type "" → "classifying…"
web/src/components/AgentDetail.tsx ~ type "" → "classifying…"; show prompt
README.md                        ~ prompt-spawn note
```

Phase order: **1** store → **2** config → **3** lifecycle → **4** daemon → **5** client → **6** GUI → **7** CLI + integration.

---

## Phase 1 — Store: Prompt field + UpdateType

### Task 1.1: Add Prompt to Session + UpdateType to the interface and MongoStore

**Files:**
- Modify: `internal/store/types.go`, `internal/store/store.go`, `internal/store/mongo.go`
- Modify: `internal/store/types_test.go`, `internal/store/mongo_test.go`
- Modify: `internal/daemon/api_test.go` (fakeStore must satisfy the grown interface)

- [ ] **Step 1: Write the failing tests**

In `internal/store/types_test.go`, add `Prompt` to the `TestSessionBSONRoundTrip` sample and assert it survives. Add these lines inside that test's `Session{...}` literal (after `Branch`):
```go
		Prompt: "do a security review of the auth module",
```
and after the existing round-trip asserts add:
```go
	require.Equal(t, "do a security review of the auth module", got.Prompt)
```

In `internal/store/mongo_test.go`, append:
```go
func TestUpdateType(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.UpdateType(ctx, "PROJ-350", TypeAnalysis))
	got, _ := st.Get(ctx, "PROJ-350")
	require.Equal(t, TypeAnalysis, got.Type)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestSessionBSONRoundTrip|TestUpdateType' 2>&1 | head`
Expected: compile error / FAIL — `Session` has no `Prompt`, `UpdateType` undefined.

- [ ] **Step 3: Add the Prompt field**

In `internal/store/types.go`, add to the `Session` struct (right after the `PR` field):
```go
	Prompt          string    `bson:"prompt" json:"prompt"`           // initial prompt (prompt-spawned agents)
```

- [ ] **Step 4: Add UpdateType to the interface**

In `internal/store/store.go`, add to the `Store` interface (after `UpdateStatus`):
```go
	UpdateType(ctx context.Context, id string, t Type) error
```

- [ ] **Step 5: Implement MongoStore.UpdateType**

In `internal/store/mongo.go`, add (next to `UpdateStatus`):
```go
func (m *MongoStore) UpdateType(ctx context.Context, id string, t Type) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"type": t})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 6: Make fakeStore satisfy the interface**

In `internal/daemon/api_test.go`, add to `fakeStore` (next to its `UpdateStatus`):
```go
func (f *fakeStore) UpdateType(_ context.Context, id string, t store.Type) error {
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Type = t
	return nil
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `make mongo-up >/dev/null 2>&1; go build ./... && go test ./internal/store/ ./internal/daemon/`
Expected: PASS (store incl. `TestUpdateType` via testcontainers; daemon still compiles + passes).

- [ ] **Step 8: Commit**

```bash
git add internal/store internal/daemon/api_test.go
git commit -m "feat: store Session.Prompt + UpdateType"
```

---

## Phase 2 — Config: Workdir

### Task 2.1: Add Workdir with ~/agentctl-agents default

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/config/config_test.go`, add:
```go
func TestWorkdirDefault(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "")
	c := Load()
	require.True(t, strings.HasSuffix(c.Workdir, "agentctl-agents"), "got %q", c.Workdir)
}

func TestWorkdirFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_WORKDIR", "/tmp/agents")
	require.Equal(t, "/tmp/agents", Load().Workdir)
}
```
Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `Config` has no `Workdir`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add `"os"` and `"path/filepath"` to imports, add the field to `Config`:
```go
	Workdir  string
```
add the helper:
```go
func defaultWorkdir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "agentctl-agents"
	}
	return filepath.Join(home, "agentctl-agents")
}
```
and add to the returned `Config{...}` in `Load`:
```go
		Workdir:  envOr("AGENTCTL_WORKDIR", defaultWorkdir()),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: config AGENTCTL_WORKDIR (default ~/agentctl-agents)"
```

---

## Phase 3 — Lifecycle: prompt-mode Spawn + Classify

### Task 3.1: shellQuoteArg + classifyArg + parseType (pure helpers, TDD)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestShellQuoteArg(t *testing.T) {
	require.Equal(t, `'hi there'`, shellQuoteArg("hi there"))
	require.Equal(t, `'a'\''b'`, shellQuoteArg("a'b"))
	require.Equal(t, "'line1\nline2'", shellQuoteArg("line1\nline2"))
}

func TestParseType(t *testing.T) {
	require.Equal(t, store.TypeDevelopment, parseType("development"))
	require.Equal(t, store.TypePRReview, parseType("pr-review\n"))
	require.Equal(t, store.TypeAnalysis, parseType("This is an analysis task."))
	require.Equal(t, store.TypeBuildkiteDebug, parseType("Label: buildkite-debug"))
	require.Equal(t, store.TypeOther, parseType("I am not sure"))
	require.Equal(t, store.TypeOther, parseType(""))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run 'TestShellQuoteArg|TestParseType'`
Expected: FAIL — `shellQuoteArg`/`parseType` undefined.

- [ ] **Step 3: Implement the helpers**

In `internal/lifecycle/lifecycle.go`, add (near the top, after `claudeCmd`):
```go
// classifyInstruction is prepended to the task prompt for headless classification.
const classifyInstruction = "You are a classifier. Classify the following agent task into exactly one of these labels: development, analysis, spike, pr-review, buildkite-debug, test-run, env-test, other. Reply with ONLY the label, nothing else.\n\nTask: "

// classifyArg builds the single argument passed to `claude -p`.
func classifyArg(prompt string) string { return classifyInstruction + prompt }

// parseType extracts the first known type label from a model's free-form reply.
func parseType(out string) store.Type {
	for _, raw := range strings.Fields(strings.ToLower(out)) {
		tok := strings.Trim(raw, ".,:;'\"`*()[]")
		if t := store.NormalizeType(tok); t != store.TypeOther || tok == "other" {
			return t
		}
	}
	return store.TypeOther
}

// shellQuoteArg single-quotes s for safe inclusion in a shell command line
// typed into a tmux pane (preserves spaces, quotes, and newlines).
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/lifecycle/ -run 'TestShellQuoteArg|TestParseType'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: lifecycle shellQuoteArg + classify prompt/parse helpers"
```

### Task 3.2: Classify (headless `claude -p`)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestClassifyCallsClaudeP(t *testing.T) {
	prompt := "build a REST API for orders"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Out: "development\n"},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.NoError(t, err)
	require.Equal(t, store.TypeDevelopment, got)
	require.Contains(t, fr.calledArgs(), []string{"claude", "-p", classifyArg(prompt)})
}

func TestClassifyDefaultsToOtherOnError(t *testing.T) {
	prompt := "whatever"
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"claude -p " + classifyArg(prompt): {Err: errStub("claude not found")},
	}}
	got, err := New(fr).Classify(context.Background(), prompt)
	require.Error(t, err)
	require.Equal(t, store.TypeOther, got)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestClassify`
Expected: FAIL — `Classify` undefined.

- [ ] **Step 3: Implement Classify**

In `internal/lifecycle/lifecycle.go`, add:
```go
// Classify asks the same Claude (headless) to label a task prompt. On any error
// it returns TypeOther alongside the error so callers can fall back gracefully.
func (l *Lifecycle) Classify(ctx context.Context, prompt string) (store.Type, error) {
	out, err := l.run.Run(ctx, "", "claude", "-p", classifyArg(prompt))
	if err != nil {
		return store.TypeOther, fmt.Errorf("claude -p: %w: %s", err, out)
	}
	return parseType(out), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/lifecycle/ -run TestClassify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: lifecycle Classify via headless claude -p"
```

### Task 3.3: Prompt-mode Spawn (no worktree, workdir, deliver prompt)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestSpawnPromptModeNoWorktree(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "research how SSE reconnection works"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Prompt: prompt, Workdir: "/home/me/agentctl-agents",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(s.ID, "agent-"), "got %q", s.ID)
	require.Equal(t, store.Type(""), s.Type, "type empty until classified")
	require.Empty(t, s.Worktree)
	require.Empty(t, s.Repo)
	require.Equal(t, prompt, s.Prompt)
	// No git at all for a prompt-spawned agent.
	for _, argv := range fr.calledArgs() {
		require.NotEqual(t, "git", argv[0], "prompt mode must not touch git")
	}
	// tmux session in the shared workdir.
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/home/me/agentctl-agents"})
	// claude launched with the prompt as a shell-quoted positional arg.
	launch := claudeCmd + " " + shellQuoteArg(prompt)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnPromptMode`
Expected: FAIL — `SpawnRequest` has no `Prompt`/`Workdir`; Spawn doesn't branch.

- [ ] **Step 3: Extend SpawnRequest + resolveID + Spawn**

In `internal/lifecycle/lifecycle.go`, add fields to `SpawnRequest`:
```go
type SpawnRequest struct {
	Type     store.Type
	Ticket   string // optional; becomes the id when present
	Repo     string
	Branch   string // optional; development branch / pr-review checkout target
	PR       string // optional; pr-review
	Worktree bool   // analysis/spike opt-in
	Prompt   string // prompt-mode: the agent's initial prompt (no repo/worktree)
	Workdir  string // prompt-mode: working directory for the tmux session
}
```

Update `resolveID` so an empty type yields an `agent-` prefix:
```go
func resolveID(req SpawnRequest) string {
	if req.Ticket != "" {
		return req.Ticket
	}
	slug := strings.ReplaceAll(string(req.Type), "-", "")
	if slug == "" {
		slug = "agent"
	}
	return slug + "-" + shortID()
}
```

Replace the `Spawn` method with the branching version:
```go
// Spawn creates an agent session. Prompt mode (Prompt set, no Type) runs a plain
// claude in Workdir with NO git worktree, seeded with the prompt. Typed mode is
// the existing per-type worktree flow (unchanged).
func (l *Lifecycle) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
	id := resolveID(req)

	sess := &store.Session{
		ID:          id,
		Type:        req.Type,
		Ticket:      req.Ticket,
		TmuxSession: id,
		Repo:        req.Repo,
		PR:          req.PR,
		Prompt:      req.Prompt,
		Status:      store.StatusSpawning,
	}

	if promptMode {
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", req.Workdir); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
		launch := claudeCmd + " " + shellQuoteArg(req.Prompt)
		if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
			return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
		}
		return sess, nil
	}

	// Typed/managed path (unchanged).
	workdir := req.Repo
	if wantWorktree(req) {
		rel := worktreeRel(id)
		branch, err := l.ensureWorktree(ctx, req, id, rel)
		if err != nil {
			return nil, err
		}
		sess.Worktree = rel
		sess.Branch = branch
		workdir = filepath.Join(req.Repo, rel)
	}
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "new-session", "-d", "-s", id, "-c", workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeCmd, "Enter"); err != nil {
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}
```

- [ ] **Step 4: Run the full lifecycle suite**

Run: `go test ./internal/lifecycle/`
Expected: PASS — the new prompt-mode test AND all existing typed/worktree Spawn tests (they pass `Type`/`Repo`, so `promptMode` is false and behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: lifecycle prompt-mode Spawn (no worktree, workdir, deliver prompt)"
```

---

## Phase 4 — Daemon: /spawn prompt path + async classify

### Task 4.1: DTO + Lifecycle.Classify + Server.workdir + NewServer + adapter

**Files:**
- Modify: `internal/daemon/api.go`, `internal/daemon/server.go`, `internal/daemon/lifecycle_adapter.go`, `internal/cli/daemon.go`
- Modify: `internal/daemon/lifecycle_routes_test.go` (fakeLife gains Classify)

- [ ] **Step 1: Extend the SpawnRequest DTO + Lifecycle interface + Server**

In `internal/daemon/api.go`, add to the `SpawnRequest` struct:
```go
	Prompt   string `json:"prompt"`   // prompt-mode: the agent's initial prompt
	Workdir  string `json:"-"`        // filled server-side in prompt mode
```
Add to the `Lifecycle` interface (after `Spawn`):
```go
	Classify(ctx context.Context, prompt string) (store.Type, error)
```
Add to the `Server` struct:
```go
	workdir string
```

- [ ] **Step 2: Update NewServer**

In `internal/daemon/server.go`, change `NewServer`:
```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, workdir string) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{store: st, life: life, poller: p, pollInterval: interval, hub: h, workdir: workdir}
}
```

- [ ] **Step 3: Map Prompt/Workdir + add Classify in the adapter**

In `internal/daemon/lifecycle_adapter.go`, update `Spawn` to pass the new fields and add `Classify`:
```go
func (a *lifecycleAdapter) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	lr := lifecycle.SpawnRequest{
		Ticket:   req.Ticket,
		Repo:     req.Repo,
		Branch:   req.Branch,
		PR:       req.PR,
		Worktree: req.Worktree,
		Prompt:   req.Prompt,
		Workdir:  req.Workdir,
	}
	// Leave Type empty in prompt mode so it stays "classifying"; otherwise normalize.
	if !(req.Prompt != "" && req.Type == "") {
		lr.Type = store.NormalizeType(req.Type)
	}
	return a.lc.Spawn(ctx, lr)
}

func (a *lifecycleAdapter) Classify(ctx context.Context, prompt string) (store.Type, error) {
	return a.lc.Classify(ctx, prompt)
}
```

- [ ] **Step 4: Wire workdir in the daemon command**

In `internal/cli/daemon.go`, add `"os"` to imports if absent, create the workdir, and pass it:
```go
			if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
				return err
			}
			runner := lifecycle.ExecRunner{}
			lc := lifecycle.New(runner)
			life := daemon.NewLifecycleAdapter(lc, st)
			pd := daemon.NewPollerDeps(st, runner)
			pl := poller.New(pd, 5*time.Minute)
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.Workdir)
```
(Keep the surrounding `cfg := config.Load()` / `ctx` / store setup as-is.)

- [ ] **Step 5: Add Classify to fakeLife**

In `internal/daemon/lifecycle_routes_test.go`, add a field + method to `fakeLife`:
```go
	classifyResult store.Type
	classified     string
```
```go
func (f *fakeLife) Classify(_ context.Context, prompt string) (store.Type, error) {
	f.classified = prompt
	if f.classifyResult == "" {
		return store.TypeOther, nil
	}
	return f.classifyResult, nil
}
```

- [ ] **Step 6: Build to verify everything compiles**

Run: `go build ./... && go vet ./...`
Expected: clean (handlers still use the old single-spawn validation — updated in Task 4.2; everything compiles).

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/api.go internal/daemon/server.go internal/daemon/lifecycle_adapter.go internal/cli/daemon.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat: daemon spawn DTO prompt/workdir, Classify iface, NewServer workdir"
```

### Task 4.2: handleSpawn prompt path + background classification

**Files:**
- Modify: `internal/daemon/lifecycle_routes.go`
- Modify: `internal/daemon/lifecycle_routes_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemon/lifecycle_routes_test.go`:
```go
func promptServer(t *testing.T, fs *fakeStore, fl *fakeLife) *Server {
	t.Helper()
	return &Server{store: fs, life: fl, hub: newHub(), workdir: "/tmp/agentctl-agents"}
}

func TestPostSpawnPromptMode(t *testing.T) {
	fl := &fakeLife{}
	srv := promptServer(t, newFakeStore(), fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Prompt: "research SSE reconnection"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.ID)
	require.Equal(t, store.Type(""), got.Type, "type empty at creation (classifying)")
	require.Equal(t, "research SSE reconnection", got.Prompt)
	require.Equal(t, "/tmp/agentctl-agents", fl.spawnedWorkdir, "server workdir passed to spawn")
}

func TestPostSpawnPromptThenClassifies(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{classifyResult: store.TypeAnalysis}
	srv := promptServer(t, fs, fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Prompt: "investigate flaky test"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	// Background classification updates the type shortly after.
	require.Eventually(t, func() bool {
		s, err := fs.Get(context.Background(), created.ID)
		return err == nil && s.Type == store.TypeAnalysis
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, "investigate flaky test", fl.classified)
}

func TestPostSpawnRequiresPromptOrTypeRepo(t *testing.T) {
	srv := promptServer(t, newFakeStore(), &fakeLife{})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{}) // nothing
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
```

This test references `fl.spawnedWorkdir`; record it in `fakeLife.Spawn`. Update `fakeLife` in the same file: add field `spawnedWorkdir string` and set it inside the existing `Spawn`:
```go
func (f *fakeLife) Spawn(_ context.Context, req SpawnRequest) (*store.Session, error) {
	f.spawnedWorkdir = req.Workdir
	id := req.Ticket
	if id == "" {
		if req.Prompt != "" && req.Type == "" {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	f.spawned = &store.Session{
		ID: id, Type: store.NormalizeTypeForFake(req), Ticket: req.Ticket,
		Repo: req.Repo, Prompt: req.Prompt, Status: store.StatusSpawning,
	}
	return f.spawned, nil
}
```
To avoid a helper that doesn't exist, replace the `Type:` line logic inline — use this exact body instead:
```go
func (f *fakeLife) Spawn(_ context.Context, req SpawnRequest) (*store.Session, error) {
	f.spawnedWorkdir = req.Workdir
	promptMode := req.Prompt != "" && req.Type == ""
	id := req.Ticket
	if id == "" {
		if promptMode {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	typ := store.Type("")
	if !promptMode {
		typ = store.NormalizeType(req.Type)
	}
	f.spawned = &store.Session{
		ID: id, Type: typ, Ticket: req.Ticket, Repo: req.Repo,
		Prompt: req.Prompt, Status: store.StatusSpawning,
	}
	return f.spawned, nil
}
```
(Delete the previous `fakeLife.Spawn` body and the earlier `spawned` assignment so only this version remains. Keep the `spawned` field on the struct.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestPostSpawnPrompt`
Expected: FAIL — handleSpawn still requires type+repo and doesn't classify.

- [ ] **Step 3: Rewrite handleSpawn + add classifyAndUpdate**

In `internal/daemon/lifecycle_routes.go`, ensure the import block has `"context"`, `"log"`, `"time"`, and `"github.com/srajanpathak/agentctl/internal/store"` (store likely already imported). Replace `handleSpawn` and add the goroutine helper:
```go
func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode && (req.Type == "" || req.Repo == "") {
		writeErr(w, http.StatusBadRequest, "provide a prompt, or type and repo")
		return
	}
	// Reject duplicate spawn on an existing ticket. No-ticket sessions get a
	// random id, so there is nothing to collide on.
	if req.Ticket != "" {
		if _, err := s.store.Get(r.Context(), req.Ticket); err == nil {
			writeErr(w, http.StatusConflict, "session already exists — use `agentctl attach "+req.Ticket+"`")
			return
		}
	}
	if promptMode {
		req.Workdir = s.workdir
	}
	sess, err := s.life.Spawn(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.Insert(r.Context(), sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	if promptMode {
		go s.classifyAndUpdate(sess.ID, req.Prompt)
	}
	writeJSON(w, http.StatusCreated, sess)
}

// classifyAndUpdate runs in the background after a prompt-spawn: it labels the
// agent's type via the LLM and updates the doc. Uses a detached context because
// the request context is already done by the time this runs.
func (s *Server) classifyAndUpdate(id, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t, err := s.life.Classify(ctx, prompt)
	if err != nil {
		t = store.TypeOther // never block: fall back to "other"
	}
	if err := s.store.UpdateType(ctx, id, t); err != nil {
		log.Printf("classify update %s: %v", id, err)
		return
	}
	s.notify()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/`
Expected: PASS — the 3 new prompt tests AND all existing spawn tests (typed-mode tests send `type`+`repo`; the `TestPostSpawnRequiresTypeAndRepo` test sends only a ticket → still 400; `TestPostSpawnNoTicketIsAllowed` sends type+repo → 201).

- [ ] **Step 5: Race check**

Run: `go test -race ./internal/daemon/`
Expected: race-clean (the background goroutine writes the fakeStore; `require.Eventually` reads it — both under the test's lifetime; if `-race` flags a fakeStore map race, that is a test-double concern: guard `fakeStore` with a `sync.Mutex` around `data` access in `api_test.go` and re-run). If a data race appears in `fakeStore`, add a `sync.Mutex mu` to `fakeStore` and lock in `Get`/`Insert`/`UpdateType`/`UpdateStatus`/`List`.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat: /spawn accepts a prompt; background classifies type via LLM"
```

---

## Phase 5 — Client: prompt in SpawnParams

### Task 5.1: client.SpawnParams.Prompt

**Files:**
- Modify: `internal/client/client.go`

- [ ] **Step 1: Add the field + send it**

In `internal/client/client.go`, add `Prompt string` to `SpawnParams` (after `Worktree`):
```go
	Prompt   string
```
and add `"prompt": p.Prompt` to the body map in `Spawn`:
```go
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt,
	}
```

- [ ] **Step 2: Verify build + existing client tests**

Run: `go build ./... && go test ./internal/client/`
Expected: PASS (existing `TestSpawn` unaffected; it doesn't assert the exact body).

- [ ] **Step 3: Commit**

```bash
git add internal/client/client.go
git commit -m "feat: client SpawnParams.Prompt"
```

---

## Phase 6 — GUI: prompt-only create + classifying label

### Task 6.1: types.ts + api.ts (TDD on api)

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/lib/api.ts`, `web/src/lib/api.test.ts`

- [ ] **Step 1: Update the failing api test**

In `web/src/lib/api.test.ts`, update the existing spawn-body test to expect the new `prompt` key, and add a prompt-only test. Replace the `it('spawn POSTs the full body to /spawn', …)` block with:
```ts
  it('spawn POSTs the full body to /spawn', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'A-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ type: 'development', repo: '/r', ticket: 'A-1' });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/spawn');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({
      type: 'development', ticket: 'A-1', repo: '/r', branch: '', pr: '', worktree: false, prompt: '',
    });
  });

  it('spawn supports a prompt-only body', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-x' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'do research on X' });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      type: '', ticket: '', repo: '', branch: '', pr: '', worktree: false, prompt: 'do research on X',
    });
  });
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — body lacks `prompt`; `spawn` requires `type`/`repo` (type error on prompt-only call).

- [ ] **Step 3: Update types + api**

In `web/src/lib/types.ts`, add to `Session` (after `pr`):
```ts
  prompt: string;
```

In `web/src/lib/api.ts`, make `SpawnParams` fields optional and add `prompt`, and send all keys with defaults:
```ts
export interface SpawnParams {
  type?: string;
  repo?: string;
  ticket?: string;
  branch?: string;
  pr?: string;
  worktree?: boolean;
  prompt?: string;
}

export async function spawn(p: SpawnParams): Promise<Session> {
  return parse<Session>(await fetch('/spawn', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      type: p.type ?? '', ticket: p.ticket ?? '', repo: p.repo ?? '',
      branch: p.branch ?? '', pr: p.pr ?? '', worktree: !!p.worktree,
      prompt: p.prompt ?? '',
    }),
  }));
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test`
Expected: PASS (both spawn tests + the rest).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/api.ts web/src/lib/api.test.ts
git commit -m "feat(web): Session.prompt + spawn supports prompt-only body"
```

### Task 6.2: NewAgentModal → single prompt textarea

**Files:**
- Modify: `web/src/components/NewAgentModal.tsx`

- [ ] **Step 1: Replace with a prompt-only modal**

Replace `web/src/components/NewAgentModal.tsx` entirely:
```tsx
import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';

export default function NewAgentModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [prompt, setPrompt] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit() {
    setErr(null);
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt });
      onCreated(s.id);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>New agent</h2>
        <label>What should this agent do?
          <textarea
            rows={6}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="e.g. Review the auth module for security issues and propose fixes…"
            autoFocus
            onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit(); }}
          />
        </label>
        <p className="muted">The type label is assigned automatically once it starts.</p>
        {err && <p className="warn">{err}</p>}
        <div className="actions">
          <button disabled={busy} onClick={submit}>Create</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 2: Add a textarea style**

In `web/src/styles/app.css`, append:
```css
.modal textarea { padding: .5rem; font: inherit; resize: vertical; }
```

- [ ] **Step 3: Typecheck + build**

Run: `cd web && npx tsc --noEmit && npm run build`
Expected: clean; `dist/` produced (postbuild keeps `.gitkeep`).

- [ ] **Step 4: Commit**

```bash
git add web/src/components/NewAgentModal.tsx web/src/styles/app.css
git commit -m "feat(web): new-agent modal is a single prompt textarea"
```

### Task 6.3: Show "classifying…" + the prompt

**Files:**
- Modify: `web/src/components/AgentList.tsx`, `web/src/components/AgentDetail.tsx`

- [ ] **Step 1: AgentList — empty type shows "classifying…"**

In `web/src/components/AgentList.tsx`, change the type cell `<td>{s.type}</td>` to:
```tsx
              <td>{s.type || <span className="muted">classifying…</span>}</td>
```

- [ ] **Step 2: AgentDetail — type label + prompt section**

In `web/src/components/AgentDetail.tsx`, update the metadata `<code>` line to handle empty type, and add a Prompt section. Replace the `<code className="muted">…</code>` line with:
```tsx
        <code className="muted">
          type: {session.type || 'classifying…'} · repo: {session.repo || '—'}{session.worktree && ` · ${session.worktree}`}
        </code>
```
And add, right after the `detail-head` `</div>` (before the "Live output" section):
```tsx
      {session.prompt && (
        <section>
          <h3>Prompt</h3>
          <p className="muted" style={{ whiteSpace: 'pre-wrap' }}>{session.prompt}</p>
        </section>
      )}
```

- [ ] **Step 3: Typecheck + build + tests**

Run: `cd web && npx tsc --noEmit && npm run build && npm test`
Expected: clean / green.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/AgentList.tsx web/src/components/AgentDetail.tsx
git commit -m "feat(web): show classifying… label + initial prompt in detail"
```

---

## Phase 7 — CLI prompt path + integration + docs

### Task 7.1: `agentctl start "<prompt>"` (no --type → prompt mode)

**Files:**
- Modify: `internal/cli/lifecycle.go`

- [ ] **Step 1: Update newStartCmd to support prompt mode**

In `internal/cli/lifecycle.go`, replace the `RunE` of `newStartCmd` with a version that branches on `--type`:
```go
		RunE: func(cmd *cobra.Command, args []string) error {
			typ, _ := cmd.Flags().GetString("type")

			// Prompt mode: `agentctl start "<prompt>"` with no --type.
			if typ == "" {
				if len(args) != 1 {
					return fmt.Errorf("provide a prompt: agentctl start \"<prompt>\"  (or use --type for a managed worktree)")
				}
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: args[0]})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "spawned %s (classifying…) — attach with `agentctl attach %s`\n", s.ID, s.ID)
				return nil
			}

			// Typed/managed worktree mode (unchanged).
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				repo = cwd
			}
			branch, _ := cmd.Flags().GetString("branch")
			pr, _ := cmd.Flags().GetString("pr")
			worktree, _ := cmd.Flags().GetBool("worktree")
			if typ == "pr-review" && pr == "" && branch == "" {
				return fmt.Errorf("pr-review needs --pr or --branch")
			}
			ticket := ""
			if len(args) == 1 {
				ticket = args[0]
			}
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
				Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "spawned %s [%s] (%s) — attach with `agentctl attach %s`\n", s.ID, s.Type, s.Status, s.ID)
			return nil
		},
```
Also update the command's `Use`/`Short` to reflect both modes:
```go
		Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>]",
		Short: "Spawn an agent — `start \"<prompt>\"` (auto-typed) or `start TICKET --type <TYPE>` (managed worktree)",
```

- [ ] **Step 2: Build + smoke (no live daemon needed for compile)**

Run: `go build ./... && go vet ./...`
Expected: clean. (`fmt`, `os`, `client` already imported in this file.)

- [ ] **Step 3: Commit**

```bash
git add internal/cli/lifecycle.go
git commit -m "feat: `agentctl start \"<prompt>\"` prompt mode (auto-typed)"
```

### Task 7.2: Integration verification + README

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Full build + suites**

Run:
```bash
make release && make mongo-up
go test ./... && go test -race ./internal/daemon/ ./internal/poller/
cd web && npm test && cd ..
```
Expected: `make release` builds; all Go packages pass; race-clean; 9 web tests pass (status 4 + api 5).

- [ ] **Step 2: Manual prompt-spawn smoke (verifies prompt delivery + classification)**

> Run the daemon on an alternate port if `:8765` is occupied. Requires `claude` installed + authenticated for real classification; without it, type falls back to `other`.
```bash
AGENTCTL_ADDR=127.0.0.1:8799 AGENTCTL_WORKDIR=/tmp/agentctl-agents ./bin/agentctl daemon & sleep 1
# prompt-only spawn via API:
curl -s -X POST localhost:8799/spawn -H 'Content-Type: application/json' \
  -d '{"prompt":"review the auth module for security issues"}'
sleep 4   # allow background `claude -p` classification
curl -s localhost:8799/sessions   # the agent should now have a non-empty "type"
# tmux pane should show claude started with the prompt:
tmux capture-pane -p -t "$(curl -s localhost:8799/sessions | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)" | tail
kill %1; tmux kill-server 2>/dev/null
```
Expected: spawn returns 201 with empty `type`; after a few seconds `/sessions` shows a classified `type`; the tmux pane shows `claude` launched with the prompt. (In a sandbox that blocks localhost, rely on the Go/web suites instead.)

- [ ] **Step 3: README — Prompt-spawn note**

In `README.md`, under the "Web GUI" section (and the CLI command reference), document:
- **New agent = just a prompt.** In the GUI, "+ New agent" is a single prompt box. On the CLI: `agentctl start "review the auth module for security issues"` (no `--type`).
- **No repo assumed.** Prompt-spawned agents run a plain `claude --dangerously-skip-permissions '<prompt>'` in `AGENTCTL_WORKDIR` (default `~/agentctl-agents`) — no git worktree. The prompt carries any repo context.
- **Type is auto-assigned** by classifying the prompt with the same Claude (`claude -p`), shortly after creation; it appears as "classifying…" until then. Requires `claude` on the daemon's PATH; falls back to `other` if unavailable.
- **Managed worktrees still available:** `agentctl start TICKET --type development --repo …` is unchanged.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: prompt-spawn + auto-type in README"
```

---

## Self-review against the spec

**Spec coverage** (`2026-06-02-agentctl-prompt-spawn-design.md`):
- §2 prompt-only create — `/spawn {prompt}` (Task 4.2), GUI textarea (6.2), CLI `start "<prompt>"` (7.1). ✅
- §2 no repo/worktree assumed — prompt-mode `Spawn` creates no worktree, runs in workdir (3.3). ✅
- §2 workdir `~/agentctl-agents` — config (Task 2.1) + `MkdirAll` + passed to server (4.1). ✅
- §2 prompt delivery — `claude --dangerously-skip-permissions '<shellQuoteArg(prompt)>'` (3.1/3.3). ✅
- §2 async classification via `claude -p` — `Classify` (3.2) + `classifyAndUpdate` goroutine (4.2) + SSE `notify`. ✅
- §2 type normalized to enum, fallback other — `parseType`/`NormalizeType` (3.1), fallback in `classifyAndUpdate` (4.2). ✅
- §2 existing typed flow kept — Spawn typed branch unchanged (3.3); CLI typed mode retained (7.1). ✅
- §4 data model — `Session.Prompt` + `UpdateType` (Task 1.1). ✅
- §4 daemon DTO/iface/server/adapter/cli wiring — Task 4.1. ✅
- §4 client + GUI — Tasks 5.1, 6.x. ✅
- §5 error handling — `claude -p` failure → other (4.2); empty prompt → 400 (4.2) + GUI disable (6.2); workdir MkdirAll (4.1); prompt-agent cleanup unchanged (no worktree → existing skip). ✅
- §6 testing — store UpdateType/Prompt (1.1), parseType/Classify/shellQuote/prompt-Spawn (3.x), daemon prompt path + async classify + validation (4.2), client/GUI (5/6). ✅
- §7 out of scope — no re-classification, no repo inference, no auth setup. ✅

**Placeholder scan:** No TBD/TODO. Every code step has complete code; every run step has expected output. The Phase 4.2 `-race` step gives the exact fakeStore-mutex remedy if the test double races (concrete, not hand-wavy).

**Type consistency:** `store.Type`/`store.NormalizeType` used uniformly. `lifecycle.SpawnRequest` gains `Prompt`/`Workdir`; the daemon `SpawnRequest` DTO gains matching `Prompt`(json) + `Workdir`(internal); the adapter maps them and leaves `Type` empty in prompt mode (so `resolveID` uses the `agent-` prefix and the doc stays "classifying"). `Lifecycle.Classify(ctx,prompt)→(store.Type,error)` is implemented by the adapter (4.1) and faked in tests (4.1); `Server.classifyAndUpdate` calls it then `store.UpdateType` (added in 1.1) then `notify()` (existing). `client.SpawnParams.Prompt` ↔ daemon `prompt` json ↔ `web` `SpawnParams.prompt`/`Session.prompt`. `NewServer` signature change (`+workdir`) has exactly one call site (`cli/daemon.go`, updated in 4.1); daemon tests build `Server{}` literals (set `workdir` where needed).
