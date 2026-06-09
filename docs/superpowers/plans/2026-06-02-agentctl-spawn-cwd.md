# Spawn Agents From the Caller's Directory — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Launch a prompt-mode agent's `claude` process from the caller's working directory (CLI shell cwd, MCP orchestrator cwd, or a web-picked folder) while keeping agentctl's per-agent data in `~/agentctl-agents/<id>`.

**Architecture:** A new `cwd` field is threaded from each spawn entry point (CLI, MCP, web) through `client` → `daemon` → `lifecycle`. In prompt mode, `lifecycle.Spawn` splits the "data dir" (`~/agentctl-agents/<id>`, holds the prompt file) from the "launch dir" (the caller's cwd, used for `tmux -c` and `sess.Workdir`); launch dir falls back to the data dir when no cwd is supplied. The web form gains a server-side directory-browser endpoint (`GET /fs/dirs`) and a mandatory Finder-style picker.

**Tech Stack:** Go (chi router, testify), React 19 + Astro, Vitest.

**Reference spec:** `docs/superpowers/specs/2026-06-02-agentctl-spawn-cwd-design.md`

---

### Task 1: `lifecycle` — `Cwd` field and launch-from-cwd in prompt mode

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (SpawnRequest struct ~148-157; prompt-mode block ~407-431)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnPromptModeLaunchesFromCwd(t *testing.T) {
	fr := &FakeRunner{}
	prompt := "fix the auth bug"
	s, err := New(fr).Spawn(context.Background(), SpawnRequest{
		Prompt: prompt, Workdir: "/home/me/agentctl-agents", Cwd: "/work/project",
	})
	require.NoError(t, err)

	dataDir := "/home/me/agentctl-agents/" + s.ID
	// Claude launches from the caller's cwd, not the data dir.
	require.Equal(t, "/work/project", s.Workdir, "sess.Workdir is the caller cwd")
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/work/project"})
	// Agent data (the prompt file) still lives under the data dir.
	require.Contains(t, fr.calledArgs(), []string{"mkdir", "-p", dataDir})
	promptFile := dataDir + "/" + promptFileName
	require.Contains(t, fr.calledArgs(), []string{"sh", "-c", `printf '%s' "$1" > "$2"`, "sh", prompt, promptFile})
	launch := claudeLaunch(s.ClaudeSessionID, s.ID) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnPromptModeLaunchesFromCwd -v`
Expected: FAIL — `unknown field 'Cwd' in struct literal` (compile error).

- [ ] **Step 3: Add the `Cwd` field to `SpawnRequest`**

In `internal/lifecycle/lifecycle.go`, in the `SpawnRequest` struct (after the `Workdir` field):

```go
	Workdir  string // prompt-mode: base dir for per-agent data (~/agentctl-agents)
	Cwd      string // prompt-mode: dir to launch claude from (caller cwd); falls back to the per-agent data dir when empty
```

- [ ] **Step 4: Split data dir from launch dir in the prompt-mode block**

In `internal/lifecycle/lifecycle.go`, replace the prompt-mode block (currently lines ~407-431) with:

```go
	if promptMode {
		dataDir := filepath.Join(req.Workdir, id)
		if out, err := l.run.Run(ctx, "", "mkdir", "-p", dataDir); err != nil {
			return nil, fmt.Errorf("mkdir workdir: %w: %s", err, out)
		}
		// Claude is launched from the caller's cwd so it operates on their project;
		// the per-agent data dir only holds bookkeeping (the prompt file). When no
		// cwd is supplied, fall back to the data dir (the original behavior).
		launchDir := req.Cwd
		if launchDir == "" {
			launchDir = dataDir
		}
		sess.Workdir = launchDir
		// Persist the prompt to a file, then launch claude with the prompt read
		// back via "$(cat …)". This keeps the command typed into the pane to a
		// single physical line: a multi-line prompt typed directly would have its
		// embedded newlines register as Enter, submitting the half-typed command.
		// The prompt is passed to the writer as an exec argument (never through a
		// shell), so quotes and newlines in it need no escaping.
		promptFile := filepath.Join(dataDir, promptFileName)
		if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", req.Prompt, promptFile); err != nil {
			return nil, fmt.Errorf("write prompt file: %w: %s", err, out)
		}
		if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", launchDir); err != nil {
			return nil, fmt.Errorf("tmux new-session: %w: %s", err, out)
		}
		launch := claudeLaunch(sess.ClaudeSessionID, id) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
		if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
			return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
		}
		return sess, nil
	}
```

- [ ] **Step 5: Run the new test and the existing prompt-mode tests**

Run: `go test ./internal/lifecycle/ -run 'TestSpawnPromptMode' -v`
Expected: PASS for `TestSpawnPromptModeLaunchesFromCwd`, `TestSpawnPromptModePerAgentWorkdir`, and `TestSpawnPromptModeNoWorktree` (the last two exercise the empty-`Cwd` fallback, where `launchDir == dataDir`, so their assertions still hold).

- [ ] **Step 6: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): launch prompt-mode agents from the caller's cwd

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `daemon` — accept, validate, and forward `cwd`

**Files:**
- Modify: `internal/daemon/api.go` (SpawnRequest struct, ~22-32)
- Modify: `internal/daemon/lifecycle_adapter.go` (Spawn translation, ~24-33)
- Modify: `internal/daemon/lifecycle_routes.go` (handleSpawn, ~25-54; imports ~3-15)
- Test: `internal/daemon/lifecycle_routes_test.go` (fakeLife, ~21-57; new tests)

- [ ] **Step 1: Write the failing tests**

First add a `spawnedCwd` field to `fakeLife` so tests can observe the forwarded value. In `internal/daemon/lifecycle_routes_test.go`, in the `fakeLife` struct add:

```go
	spawnedWorkdir string
	spawnedCwd     string
```

And in `fakeLife.Spawn`, after `f.spawnedWorkdir = req.Workdir`, add:

```go
	f.spawnedCwd = req.Cwd
```

Then add these tests to the same file:

```go
func TestPostSpawnForwardsCwd(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	dir := t.TempDir()
	body, _ := json.Marshal(SpawnRequest{Prompt: "do X", Cwd: dir})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, dir, fl.spawnedCwd, "cwd is forwarded to the lifecycle")
}

func TestPostSpawnRejectsMissingCwd(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Prompt: "do X", Cwd: "/no/such/dir/xyz123"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a cwd that isn't an existing dir is rejected")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestPostSpawnForwardsCwd|TestPostSpawnRejectsMissingCwd' -v`
Expected: FAIL — `unknown field 'Cwd'` (compile error).

- [ ] **Step 3: Add `Cwd` to the daemon `SpawnRequest`**

In `internal/daemon/api.go`, in the `SpawnRequest` struct (after the `Workdir` field):

```go
	Workdir  string `json:"-"`        // filled server-side in prompt mode
	Cwd      string `json:"cwd"`      // dir to launch claude from (caller cwd / web pick)
```

- [ ] **Step 4: Forward `Cwd` through the adapter**

In `internal/daemon/lifecycle_adapter.go`, in `Spawn`, add `Cwd` to the `lifecycle.SpawnRequest` literal:

```go
	lr := lifecycle.SpawnRequest{
		Ticket:   req.Ticket,
		Repo:     req.Repo,
		Branch:   req.Branch,
		PR:       req.PR,
		Worktree: req.Worktree,
		Prompt:   req.Prompt,
		Workdir:  req.Workdir,
		Cwd:      req.Cwd,
	}
```

- [ ] **Step 5: Validate `Cwd` in `handleSpawn`**

In `internal/daemon/lifecycle_routes.go`, add `"os"` to the import block. Then in `handleSpawn`, immediately before the `if promptMode { req.Workdir = s.workdir }` line, insert:

```go
	// A supplied cwd must be a real directory (guards the web picker / typos).
	// Empty cwd is allowed — the lifecycle falls back to the per-agent data dir.
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			writeErr(w, http.StatusBadRequest, "cwd is not an existing directory: "+req.Cwd)
			return
		}
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestPostSpawn' -v`
Expected: PASS (new cwd tests plus the existing `TestPostSpawn*` tests).

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): accept, validate, and forward spawn cwd

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `client` + CLI — capture cwd, add `--dir`

**Files:**
- Modify: `internal/client/client.go` (SpawnParams ~107-115; Spawn body ~117-128)
- Modify: `internal/cli/lifecycle.go` (imports; start command prompt-mode ~20-31; flags ~62-66)
- Test: `internal/cli/lifecycle_test.go` (new file)

- [ ] **Step 1: Write the failing test for the dir-resolution helper**

Create `internal/cli/lifecycle_test.go`:

```go
package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveDir(t *testing.T) {
	got, err := resolveDir("/explicit/path")
	require.NoError(t, err)
	require.Equal(t, "/explicit/path", got, "an explicit --dir wins")

	wd, err := os.Getwd()
	require.NoError(t, err)
	got, err = resolveDir("")
	require.NoError(t, err)
	require.Equal(t, wd, got, "empty --dir falls back to the current directory")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestResolveDir -v`
Expected: FAIL — `undefined: resolveDir`.

- [ ] **Step 3: Add the `Cwd` field to `client.SpawnParams` and send it**

In `internal/client/client.go`, add to `SpawnParams`:

```go
	Worktree bool
	Prompt   string
	Cwd      string
```

And in `Spawn`, add `cwd` to the body map:

```go
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt, "cwd": p.Cwd,
	}
```

- [ ] **Step 4: Add `resolveDir` and wire `--dir` into the start command**

In `internal/cli/lifecycle.go`, add `resolveDir` (place it just below `newStartCmd`):

```go
// resolveDir returns the explicit --dir flag value, or the current working
// directory when the flag is empty. This is where the agent's claude is launched.
func resolveDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	return os.Getwd()
}
```

In the prompt-mode branch of `newStartCmd` (currently lines ~20-31), replace the spawn call so it resolves and passes the directory:

```go
		// Prompt mode: `agentctl start "<prompt>"` with no --type.
		if typ == "" {
			if len(args) != 1 {
				return fmt.Errorf("provide a prompt: agentctl start \"<prompt>\"  (or use --type for a managed worktree)")
			}
			dirFlag, _ := cmd.Flags().GetString("dir")
			dir, err := resolveDir(dirFlag)
			if err != nil {
				return err
			}
			s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: args[0], Cwd: dir})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "spawned %s (classifying…) — attach with `agentctl attach %s`\n", s.ID, s.ID)
			return nil
		}
```

Then register the flag alongside the others (after the `worktree` flag, ~line 66):

```go
	cmd.Flags().String("dir", "", "directory to launch the agent from (default: current directory)")
```

(`os` is already imported in this file.)

- [ ] **Step 5: Run the test and build**

Run: `go test ./internal/cli/ -run TestResolveDir -v && go build ./...`
Expected: PASS, and the build succeeds.

- [ ] **Step 6: Commit**

```bash
git add internal/client/client.go internal/cli/lifecycle.go internal/cli/lifecycle_test.go
git commit -m "feat(cli): default agent launch dir to cwd, add --dir override

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: MCP — default cwd to the orchestrator's working dir, add `dir`

**Files:**
- Modify: `internal/mcp/server.go` (imports ~3-9; spawnArgs ~25-33; spawn_agent handler ~89-103)
- Test: `internal/mcp/server_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/mcp/server_test.go`:

```go
func TestSpawnAgentToolSendsExplicitDirAsCwd(t *testing.T) {
	var gotBody string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/spawn" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"agent-x","status":"spawning"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "spawn_agent",
		Arguments: map[string]any{"prompt": "do X", "dir": "/work/proj"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Contains(t, gotBody, `"cwd":"/work/proj"`)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestSpawnAgentToolSendsExplicitDirAsCwd -v`
Expected: FAIL — `unknown field 'Dir'` (compile error) once the handler is referenced, or an assertion failure that `gotBody` lacks `"cwd"`.

- [ ] **Step 3: Add `Dir` to `spawnArgs` and `"os"` to imports**

In `internal/mcp/server.go`, add `"os"` to the import block. Then add to the `spawnArgs` struct (after `Prompt`):

```go
	Prompt   string `json:"prompt,omitempty" jsonschema:"what the agent should do — prompt-mode: auto-typed, no repo needed"`
	Dir      string `json:"dir,omitempty" jsonschema:"directory to launch the agent from; defaults to the orchestrator's current working directory"`
```

- [ ] **Step 4: Resolve and pass cwd in the spawn_agent handler**

In `internal/mcp/server.go`, replace the body of the `spawn_agent` handler (the function passed to `AddTool`, ~92-103) with:

```go
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a spawnArgs) (*mcpsdk.CallToolResult, any, error) {
		cwd := a.Dir
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: a.Type, Ticket: a.Ticket, Repo: a.Repo,
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree,
			Prompt: a.Prompt, Cwd: cwd,
		})
		if err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		res, err := jsonResult(sess)
		return res, nil, err
	})
```

- [ ] **Step 5: Run the MCP tests**

Run: `go test ./internal/mcp/ -v`
Expected: PASS (the new test and the existing `TestSpawnAgentToolSendsPrompt`, which is unaffected because `cwd` is an additional field).

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): default spawn cwd to orchestrator dir, add dir arg

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `daemon` — `GET /fs/dirs` directory-browser endpoint

**Files:**
- Create: `internal/daemon/fs_routes.go`
- Modify: `internal/daemon/api.go` (router, ~103-115)
- Test: `internal/daemon/fs_routes_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/fs_routes_test.go`:

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListDirsListsSubdirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "alpha"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "beta"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".hidden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644))

	ts := httptest.NewServer((&Server{}).router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/fs/dirs?path=" + url.QueryEscape(root))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out DirListing
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, root, out.Path)
	require.Equal(t, filepath.Dir(root), out.Parent)
	names := []string{}
	for _, e := range out.Entries {
		names = append(names, e.Name)
	}
	require.Equal(t, []string{"alpha", "beta"}, names, "subdirs only, sorted, no hidden, no files")
}

func TestListDirsRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))

	ts := httptest.NewServer((&Server{}).router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/fs/dirs?path=" + url.QueryEscape(f))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestListDirs -v`
Expected: FAIL — `undefined: DirListing` (compile error).

- [ ] **Step 3: Create the endpoint**

Create `internal/daemon/fs_routes.go`:

```go
package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dirEntry is one subdirectory in a DirListing.
type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DirListing is the body of GET /fs/dirs: the resolved directory, its parent
// (empty at the filesystem root), and its immediate subdirectories.
type DirListing struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []dirEntry `json:"entries"`
}

// handleListDirs lists the immediate subdirectories of ?path= (defaulting to the
// user's home directory). It powers the web "new agent" directory picker, which
// cannot use a native folder dialog (browsers hide absolute paths).
func (s *Server) handleListDirs(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "no home directory: "+err.Error())
			return
		}
		path = home
	}
	path = filepath.Clean(path)
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "not a directory: "+path)
		return
	}
	items, err := os.ReadDir(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read directory: "+err.Error())
		return
	}
	entries := []dirEntry{}
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		entries = append(entries, dirEntry{Name: it.Name(), Path: filepath.Join(path, it.Name())})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // already at the filesystem root
	}
	writeJSON(w, http.StatusOK, DirListing{Path: path, Parent: parent, Entries: entries})
}
```

- [ ] **Step 4: Register the route**

In `internal/daemon/api.go`, in `router()`, add the `/fs/dirs` route after the lifecycle routes and before `s.registerStatic(r)`:

```go
	s.registerLifecycleRoutes(r)
	r.Get("/fs/dirs", s.handleListDirs)
	s.registerStatic(r) // catch-all; must be last
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestListDirs -v`
Expected: PASS for both.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/fs_routes.go internal/daemon/fs_routes_test.go internal/daemon/api.go
git commit -m "feat(daemon): add GET /fs/dirs directory-browser endpoint

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Web — `cwd` in spawn, `listDirs`, and the mandatory picker

**Files:**
- Modify: `web/src/lib/api.ts` (SpawnParams ~10-18; spawn body ~41-50; add DirListing types + listDirs)
- Modify: `web/src/lib/api.test.ts` (existing spawn assertions; new tests)
- Create: `web/src/components/DirPicker.tsx`
- Modify: `web/src/components/NewAgentModal.tsx`

- [ ] **Step 1: Write the failing api tests**

In `web/src/lib/api.test.ts`, update the two existing spawn assertions to include the new `cwd` field, and add `listDirs` import + tests.

Change the import line to:

```ts
import { listSessions, spawn, listDirs, cleanup, ApiError } from './api';
```

In the test `'spawn POSTs the full body to /spawn'`, change the expected object to:

```ts
    expect(JSON.parse(opts.body)).toEqual({
      type: 'development', ticket: 'A-1', repo: '/r', branch: '', pr: '', worktree: false, prompt: '', cwd: '',
    });
```

In the test `'spawn supports a prompt-only body'`, change the expected object to:

```ts
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      type: '', ticket: '', repo: '', branch: '', pr: '', worktree: false, prompt: 'do research on X', cwd: '',
    });
```

Then add two new tests inside the `describe('api', …)` block:

```ts
  it('spawn includes cwd when provided', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-x' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'do X', cwd: '/work/project' });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).cwd).toBe('/work/project');
  });

  it('listDirs GETs /fs/dirs with the path query', async () => {
    const listing = { path: '/work', parent: '/', entries: [{ name: 'project', path: '/work/project' }] };
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(listing));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listDirs('/work');
    expect(fetchMock).toHaveBeenCalledWith('/fs/dirs?path=%2Fwork');
    expect(out.entries[0].path).toBe('/work/project');
  });
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npm test`
Expected: FAIL — `listDirs` is not exported, and the two updated spawn assertions fail because the body has no `cwd` yet.

- [ ] **Step 3: Add `cwd` to spawn and the `listDirs` API**

In `web/src/lib/api.ts`, add `cwd` to `SpawnParams`:

```ts
export interface SpawnParams {
  type?: string;
  repo?: string;
  ticket?: string;
  branch?: string;
  pr?: string;
  worktree?: boolean;
  prompt?: string;
  cwd?: string;
}
```

Add `cwd` to the `spawn` body:

```ts
    body: JSON.stringify({
      type: p.type ?? '', ticket: p.ticket ?? '', repo: p.repo ?? '',
      branch: p.branch ?? '', pr: p.pr ?? '', worktree: !!p.worktree,
      prompt: p.prompt ?? '', cwd: p.cwd ?? '',
    }),
```

Add the directory types and `listDirs` function (place after the `spawn` function):

```ts
export interface DirEntry { name: string; path: string; }
export interface DirListing { path: string; parent: string; entries: DirEntry[]; }

export async function listDirs(path?: string): Promise<DirListing> {
  const q = path ? `?path=${encodeURIComponent(path)}` : '';
  return parse<DirListing>(await fetch(`/fs/dirs${q}`));
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npm test`
Expected: PASS (all api tests).

- [ ] **Step 5: Create the DirPicker component**

Create `web/src/components/DirPicker.tsx`:

```tsx
import { useEffect, useState } from 'react';
import { listDirs, type DirListing } from '../lib/api';

export default function DirPicker({ value, onChange }: {
  value: string | null;
  onChange: (path: string) => void;
}) {
  const [listing, setListing] = useState<DirListing | null>(null);
  const [err, setErr] = useState<string | null>(null);

  async function load(path?: string) {
    setErr(null);
    try {
      setListing(await listDirs(path));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  useEffect(() => { void load(); }, []);

  return (
    <div className="dirpicker">
      <div className="dirpicker-path muted">{listing?.path ?? 'loading…'}</div>
      {err && <p className="warn">{err}</p>}
      <ul className="dirpicker-list">
        {listing?.parent && (
          <li><button type="button" onClick={() => void load(listing.parent)}>../</button></li>
        )}
        {listing?.entries.map((e) => (
          <li key={e.path}><button type="button" onClick={() => void load(e.path)}>{e.name}/</button></li>
        ))}
      </ul>
      <button
        type="button"
        className="dirpicker-use"
        disabled={!listing}
        onClick={() => listing && onChange(listing.path)}
      >
        Use this folder{listing && value === listing.path ? ' ✓' : ''}
      </button>
      {value && <p className="muted">Selected: {value}</p>}
    </div>
  );
}
```

- [ ] **Step 6: Wire the picker into NewAgentModal**

Replace `web/src/components/NewAgentModal.tsx` with:

```tsx
import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';
import DirPicker from './DirPicker';

export default function NewAgentModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [prompt, setPrompt] = useState('');
  const [dir, setDir] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit() {
    setErr(null);
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
    if (!dir) { setErr('choose a directory to launch the agent from'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt, cwd: dir });
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
        <label>Launch directory
          <DirPicker value={dir} onChange={setDir} />
        </label>
        <p className="muted">The type label is assigned automatically once it starts.</p>
        {err && <p className="warn">{err}</p>}
        <div className="actions">
          <button disabled={busy || !dir} onClick={submit}>Create</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] **Step 7: Build the web UI to typecheck the components**

Run: `cd web && npm run build`
Expected: build succeeds (Astro compiles the React components with no type errors).

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/api.ts web/src/lib/api.test.ts web/src/components/DirPicker.tsx web/src/components/NewAgentModal.tsx
git commit -m "feat(web): mandatory launch-dir picker in the new-agent form

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the whole Go test suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 2: Run the web test suite and build**

Run: `cd web && npm test && npm run build`
Expected: PASS, build succeeds.

- [ ] **Step 3: Manual smoke test (CLI)**

From a project directory, with the daemon running:

```bash
cd /some/project
agentctl start "list the files you can see and summarize this repo"
agentctl attach <printed-id>   # confirm tmux opened in /some/project and claude can see the repo
```

Expected: the agent's tmux session is in `/some/project`; `~/agentctl-agents/<id>/.agentctl-prompt` exists.

---

## Notes for the implementer

- **Empty-cwd fallback is intentional.** When no cwd reaches the lifecycle, `launchDir` falls back to the per-agent data dir — preserving the original behavior. The existing `TestSpawnPromptModePerAgentWorkdir` / `TestSpawnPromptModeNoWorktree` tests exercise this path and must keep passing unchanged.
- **Typed/worktree mode is untouched.** It already runs in the repo (CLI defaults `--repo` to cwd). Do not thread `cwd` into the typed path.
- **`sess.Workdir` is the launch dir, by design.** It feeds `claudeProjectDir` (transcript lookup for `--session-id`/`restore`) and `Restore` (which recreates tmux there), so pointing it at the real project keeps both correct.
