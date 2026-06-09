# Supervised Spawn Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in per-spawn `supervised` flag that launches an agent with `claude --permission-mode acceptEdits` (auto-approves edits, PROMPTS for other tools) instead of the default `--dangerously-skip-permissions` — so a supervised agent produces the numbered tool-permission menus the approvals inbox can answer.

**Architecture:** A single `bool` ("supervised") threaded through every spawn edge exactly like the existing `Worktree bool` (client.SpawnParams → daemon SpawnRequest → adapter → lifecycle.SpawnRequest), persisted on `store.Session` so `Restore` re-launches in the same mode. `internal/lifecycle` centralizes the permission flag in one builder; default behavior is unchanged (bypass).

**Tech Stack:** Go (chi, cobra, testify, modelcontextprotocol sdk), TypeScript/React 19 (Astro web, vitest).

**Design decisions (locked):** mode = `acceptEdits`; opt-in per spawn (default stays `bypassPermissions`); store as `Supervised bool` (mode string centralized in one builder); minimal indicator (CLI `status` detail + web detail + TUI detail — no `ls` column reflow).

**Why:** agentctl spawns every agent with `--dangerously-skip-permissions`, which suppresses the permission menus the approvals inbox (`internal/approval`) is built to answer. Supervised mode is the opt-in that makes the inbox useful. See `internal/lifecycle/lifecycle.go:37`.

---

## File Structure

**Modified files:**
- `internal/store/types.go` — `Session.Supervised bool`.
- `internal/lifecycle/lifecycle.go` — `permissionFlag`/`claudeBase` builder; `SpawnRequest.Supervised`; thread through `claudeLaunch`/`claudeResume`/`resumeInTmux`/`Spawn`/`Restore`/`Adopt`.
- `internal/daemon/api.go` — `SpawnRequest.Supervised`.
- `internal/daemon/lifecycle_adapter.go` — map `req.Supervised`.
- `internal/client/client.go` — `SpawnParams.Supervised` + body field.
- `internal/cli/lifecycle.go` — `--supervised` flag on `start`.
- `internal/mcp/server.go` — `spawnArgs.Supervised` + pass-through + tool description.
- `internal/cli/sessions.go` — `supervised: yes/no` line in `status` detail.
- `internal/tui/detail.go` — supervised line in `renderDetail`.
- `web/src/lib/types.ts` — `Session.supervised`.
- `web/src/lib/api.ts` — `SpawnParams.supervised` + body.
- `web/src/components/QuickSpawn.tsx` + `NewAgentModal.tsx` — supervised checkbox.
- `web/src/components/*` (agent detail) — supervised pill.
- `README.md` — document supervised mode.

---

## Task 1: `store.Session.Supervised`

**Files:**
- Modify: `internal/store/types.go`
- Test: `internal/store/file_test.go` (or the existing store test file)

- [ ] **Step 1: Write the failing test**

Find the existing store round-trip test (e.g. `TestFileStoreInsertGet` or similar in the store test file) for the pattern. Add:

```go
func TestSupervisedRoundTrips(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, fs.Insert(context.Background(), &Session{ID: "a1", Supervised: true}))
	got, err := fs.Get(context.Background(), "a1")
	require.NoError(t, err)
	require.True(t, got.Supervised)
}
```

(Match the package's existing store-construction helper if it differs from `NewFileStore(dir)`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSupervisedRoundTrips -v`
Expected: FAIL — `Supervised` undefined.

- [ ] **Step 3: Add the field**

In `internal/store/types.go`, add to the `Session` struct (after `PR` or near the other spawn-option fields):

```go
	Supervised      bool      `json:"supervised"` // launched with --permission-mode acceptEdits (prompts) instead of bypass
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSupervisedRoundTrips -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/types.go internal/store/file_test.go
git commit -m "feat(store): persist Session.Supervised"
```

---

## Task 2: lifecycle — permission-flag builder + thread `Supervised`

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestClaudeLaunchPermissionMode(t *testing.T) {
	def := claudeLaunch("sid", "agent-1", false)
	require.Contains(t, def, "--dangerously-skip-permissions")
	require.NotContains(t, def, "--permission-mode")

	sup := claudeLaunch("sid", "agent-1", true)
	require.Contains(t, sup, "--permission-mode acceptEdits")
	require.NotContains(t, sup, "--dangerously-skip-permissions")
}

func TestClaudeResumePermissionMode(t *testing.T) {
	require.Contains(t, claudeResume("sid", "agent-1", false), "--dangerously-skip-permissions")
	require.Contains(t, claudeResume("sid", "agent-1", true), "--permission-mode acceptEdits")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lifecycle/ -run 'TestClaudeLaunchPermissionMode|TestClaudeResumePermissionMode' -v`
Expected: FAIL — `claudeLaunch`/`claudeResume` take 2 args, not 3.

- [ ] **Step 3: Implement the builder + update both builders**

In `internal/lifecycle/lifecycle.go`, REMOVE the `const claudeCmd = "claude --dangerously-skip-permissions"` line and its doc comment, and replace with:

```go
// permissionFlag selects the claude permission mode for a spawned agent.
// Supervised agents run --permission-mode acceptEdits: file edits + common FS
// commands auto-approve, but other tools (bash writes, network) PROMPT with the
// numbered menu the approvals inbox answers. The default is fully autonomous
// (--dangerously-skip-permissions / bypass) — no prompts.
func permissionFlag(supervised bool) string {
	if supervised {
		return "--permission-mode acceptEdits"
	}
	return "--dangerously-skip-permissions"
}

// claudeBase is the claude command + permission flag every agent session starts from.
func claudeBase(supervised bool) string { return "claude " + permissionFlag(supervised) }
```

Update `claudeLaunch` and `claudeResume` to take `supervised bool` and build from `claudeBase`:

```go
func claudeLaunch(sessionID, name string, supervised bool) string {
	return claudeBase(supervised) + " --session-id " + sessionID + " --name " + shellQuoteArg(name)
}

func claudeResume(sessionID, name string, supervised bool) string {
	return claudeBase(supervised) + " --resume " + sessionID + " --name " + shellQuoteArg(name)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/ -run 'TestClaudeLaunchPermissionMode|TestClaudeResumePermissionMode' -v`
Expected: PASS. (The package won't fully build yet — the call sites still pass 2 args. Fix them in Step 5.)

- [ ] **Step 5: Thread `Supervised` through SpawnRequest, Spawn, resume, Restore, Adopt**

In `internal/lifecycle/lifecycle.go`:

1. Add to `SpawnRequest`:
```go
	Supervised bool // opt-in: launch with --permission-mode acceptEdits (prompts) instead of bypass
```

2. In `Spawn`, where the `sess` record's fields are set, set `sess.Supervised = req.Supervised` (do this near where `sess.Workdir`/other fields are assigned, so it is persisted and available to Restore).

3. Update the two `claudeLaunch` call sites in `Spawn` to pass `req.Supervised`:
   - prompt-mode: `launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + ...`
   - typed mode: `..., claudeLaunch(sess.ClaudeSessionID, id, req.Supervised), "Enter")`

4. Change `resumeInTmux` to take `supervised bool` and pass it to `claudeResume`:
```go
func (l *Lifecycle) resumeInTmux(ctx context.Context, id, cwd, claudeID string, supervised bool) error {
	if err := l.newAgentSession(ctx, "", id, cwd); err != nil {
		return err
	}
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, claudeResume(claudeID, id, supervised), "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
	return nil
}
```

5. Update `resumeInTmux` callers:
   - In `Restore`: `return l.resumeInTmux(ctx, sess.ID, sess.Workdir, sess.ClaudeSessionID, sess.Supervised)`
   - In `Adopt` (resume-mode call): pass `false` (adopted agents default to non-supervised). Find the `resumeInTmux(...)` call in `Adopt` and append `, false`.

- [ ] **Step 6: Add a Spawn test that the session records Supervised**

Add to `internal/lifecycle/lifecycle_test.go` (mirror an existing prompt-mode `Spawn` test's `FakeRunner` + `Lifecycle` setup — copy that harness exactly):

```go
func TestSpawnRecordsSupervised(t *testing.T) {
	// Mirror the existing prompt-mode Spawn test setup (FakeRunner, Lifecycle with
	// PromptsDir set). Spawn a prompt-mode agent with Supervised:true.
	l, _ := newTestLifecycle(t) // use whatever constructor the existing Spawn tests use
	sess, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "do x", Cwd: t.TempDir(), Supervised: true})
	require.NoError(t, err)
	require.True(t, sess.Supervised)
}
```

If there is no reusable `newTestLifecycle`/prompt-mode Spawn test to copy, instead assert at the command level: capture the `FakeRunner` calls and assert the `send-keys` launch line for a supervised spawn contains `--permission-mode acceptEdits`. Use whichever matches the existing test style — do NOT invent a new runner.

- [ ] **Step 7: Run lifecycle tests + build**

Run: `go test ./internal/lifecycle/ -v && go build ./...`
Expected: PASS, clean build. (The daemon `fakeLife`/adapter still compile — `SpawnRequest` only gained a field.)

- [ ] **Step 8: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): supervised spawn → --permission-mode acceptEdits"
```

---

## Task 3: daemon `SpawnRequest.Supervised` + adapter

**Files:**
- Modify: `internal/daemon/api.go`, `internal/daemon/lifecycle_adapter.go`
- Test: `internal/daemon/lifecycle_routes_test.go`

- [ ] **Step 1: Write the failing test**

The `fakeLife.Spawn` records the daemon `SpawnRequest` into `fl.spawned` (verify this in `lifecycle_routes_test.go`; the existing `TestPostSpawn` uses `fl.spawned`). Add:

```go
func TestPostSpawnSupervised(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo", Supervised: true})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.True(t, fl.spawned.Supervised)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestPostSpawnSupervised -v`
Expected: FAIL — `SpawnRequest` has no `Supervised`.

- [ ] **Step 3: Add the field + adapter mapping**

In `internal/daemon/api.go`, add to `SpawnRequest`:
```go
	Supervised bool `json:"supervised"` // opt-in supervised mode (acceptEdits prompts)
```

In `internal/daemon/lifecycle_adapter.go`, in `Spawn`, add to the `lifecycle.SpawnRequest{...}` literal:
```go
		Supervised: req.Supervised,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestPostSpawnSupervised -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): thread Supervised through SpawnRequest + adapter"
```

---

## Task 4: client `SpawnParams.Supervised`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Add (mirror the existing client Spawn test's httptest body-capture):

```go
func TestClientSpawnSendsSupervised(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"a1"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Spawn(context.Background(), SpawnParams{Prompt: "x", Supervised: true})
	require.NoError(t, err)
	require.Equal(t, true, got["supervised"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestClientSpawnSendsSupervised -v`
Expected: FAIL — `SpawnParams` has no `Supervised`.

- [ ] **Step 3: Implement**

In `internal/client/client.go`, add to `SpawnParams`:
```go
	Supervised bool
```
And add to the `Spawn` body map:
```go
		"prompt": p.Prompt, "cwd": p.Cwd, "supervised": p.Supervised,
```
(Replace the existing `"prompt": p.Prompt, "cwd": p.Cwd,` line so `supervised` is included.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run TestClientSpawnSendsSupervised -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): SpawnParams.Supervised in spawn body"
```

---

## Task 5: CLI `--supervised` flag

**Files:**
- Modify: `internal/cli/lifecycle.go`
- Test: `internal/cli/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Find the existing `start`-command test that asserts a flag flows into `SpawnParams` (mirror it; if the CLI tests use a fake client capturing the last `SpawnParams`, use that). Add a test that `agentctl start "do x" --supervised` results in `SpawnParams.Supervised == true`. Concrete shape (adapt to the file's harness):

```go
func TestStartSupervisedFlag(t *testing.T) {
	fc := &fakeClient{} // the test double the other start tests use
	cmd := newStartCmd() // or however the suite builds the command with fc injected
	cmd.SetArgs([]string{"do x", "--supervised"})
	require.NoError(t, cmd.Execute())
	require.True(t, fc.lastSpawn.Supervised)
}
```

If the CLI test suite has no such fake-client harness, assert instead via a smaller unit (e.g. the flag is registered and parsed) consistent with how `--worktree` is tested — do not invent new infrastructure.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestStartSupervised -v`
Expected: FAIL — flag unknown / Supervised not set.

- [ ] **Step 3: Implement**

In `internal/cli/lifecycle.go`, in the `start` command:
1. Register the flag (near the other `cmd.Flags()` registrations):
```go
	cmd.Flags().Bool("supervised", false, "launch in acceptEdits mode (prompts for risky tools → answerable in the approvals inbox)")
```
2. Read it in `RunE` (near where `worktree`/`dir` are read):
```go
			supervised, _ := cmd.Flags().GetBool("supervised")
```
3. Pass it in BOTH `SpawnParams` constructions (prompt-mode AND typed-mode):
   - prompt-mode: `client.SpawnParams{Prompt: args[0], Cwd: dir, Supervised: supervised}`
   - typed-mode: add `Supervised: supervised,` to the `SpawnParams{Type: typ, ...}` literal.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestStartSupervised -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/lifecycle.go internal/cli/lifecycle_test.go
git commit -m "feat(cli): agentctl start --supervised"
```

---

## Task 6: MCP `spawn_agent` supervised arg

**Files:**
- Modify: `internal/mcp/server.go`
- Test: `internal/mcp/server_test.go` (if present; otherwise build-only)

- [ ] **Step 1: Write the failing test (if the MCP suite tests spawn args)**

Inspect `internal/mcp/server_test.go`. If it has a test that drives `spawn_agent` against a fake client capturing `SpawnParams`, add a case asserting `supervised:true` flows to `SpawnParams.Supervised`. If there is no such harness, SKIP the test and rely on build + the final review (note this in your report).

- [ ] **Step 2: Implement**

In `internal/mcp/server.go`:
1. Add to `spawnArgs`:
```go
	Supervised bool   `json:"supervised,omitempty" jsonschema:"supervised mode: launch with --permission-mode acceptEdits so risky tools prompt (answerable in the approvals inbox) instead of bypassing all permissions"`
```
2. Pass it in the `client.SpawnParams{...}` literal in the `spawn_agent` handler:
```go
			Prompt: a.Prompt, Cwd: cwd, Supervised: a.Supervised,
```
(Replace the existing `Prompt: a.Prompt, Cwd: cwd,` line.)
3. Update the `spawn_agent` tool `Description`: change the trailing "Launches claude --dangerously-skip-permissions." to "Launches claude --dangerously-skip-permissions by default; set supervised=true for --permission-mode acceptEdits (risky tools prompt → answerable in the approvals inbox)."

- [ ] **Step 3: Run / build**

Run: `go test ./internal/mcp/ -v && go build ./...`
Expected: PASS (or build-clean if no spawn-arg test exists).

- [ ] **Step 4: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): spawn_agent supervised arg"
```

---

## Task 7: web — `supervised` in api + spawn forms

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/lib/api.ts`, `web/src/components/QuickSpawn.tsx`, `web/src/components/NewAgentModal.tsx`
- Test: `web/src/lib/api.test.ts`

- [ ] **Step 1: Write the failing test**

Add to `web/src/lib/api.test.ts` (mirror the existing spawn test):

```ts
it('spawn sends supervised', async () => {
  const f = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: 'a1' }) });
  globalThis.fetch = f as any;
  await spawn({ prompt: 'x', supervised: true });
  const body = JSON.parse((f.mock.calls[0][1] as any).body);
  expect(body.supervised).toBe(true);
});
```

(Ensure `spawn` is imported in the test file — it is used by existing tests.)

- [ ] **Step 2: Run test to verify it fails**

Run (from `web/`): `npm test -- api.test`
Expected: FAIL — `supervised` not in body / not in `SpawnParams`.

- [ ] **Step 3: Implement**

In `web/src/lib/api.ts`, add `supervised?: boolean;` to the `SpawnParams` interface, and add to the spawn body:
```ts
      prompt: p.prompt ?? '', cwd: p.cwd ?? '', supervised: !!p.supervised,
```
(Replace the existing `prompt: p.prompt ?? '', cwd: p.cwd ?? '',` line.)

In `web/src/lib/types.ts`, add to `Session`:
```ts
  supervised: boolean;
```

In `web/src/components/QuickSpawn.tsx` and `web/src/components/NewAgentModal.tsx`, add a `supervised` checkbox:
- Add state: `const [supervised, setSupervised] = useState(false);`
- Add the control inside the form (near the dir picker), e.g.:
```tsx
        <label className="supervised-toggle">
          <input type="checkbox" checked={supervised} onChange={(e) => setSupervised((e.target as HTMLInputElement).checked)} />
          Supervised (prompts for risky tools — answer in the inbox)
        </label>
```
- Pass it to spawn: change `spawn({ prompt, cwd: dir })` → `spawn({ prompt, cwd: dir, supervised })` in BOTH components.

- [ ] **Step 4: Run test + build**

Run (from `web/`): `npm test -- api.test` (PASS), then `npm test` (full green), `npx tsc --noEmit` (clean), `npm run build` (clean).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/api.ts web/src/components/QuickSpawn.tsx web/src/components/NewAgentModal.tsx web/src/lib/api.test.ts
git commit -m "feat(web): supervised checkbox + spawn body"
```

---

## Task 8: minimal supervised indicator (CLI status + TUI detail + web detail)

**Files:**
- Modify: `internal/cli/sessions.go`, `internal/tui/detail.go`, and the web agent-detail component.
- Test: `internal/tui/detail_test.go` if present (else covered by build); CLI/web by inspection.

- [ ] **Step 1: CLI `status` detail line**

In `internal/cli/sessions.go`, the `status` detail uses one big `fmt.Fprintf(out, "id: ...\nworktree: %s\n...", ...)`. Add a `supervised:` line to that format string and a corresponding arg. Insert after the `pr:` line:
- format: add `supervised: %v\n` in the appropriate spot
- arg: add `s.Supervised` at the matching position.
Verify the format-string verb count matches the arg count after editing.

- [ ] **Step 2: TUI detail line**

In `internal/tui/detail.go` `renderDetail`, where `meta`/`subj` lines are built, add a supervised line shown ONLY when true (keep noise low):

```go
	if s.Supervised {
		meta += "  " + stMuted.Render("· supervised")
	}
```
(Append to the existing `meta` line; `s` is the `*store.Session`.)

- [ ] **Step 3: Web detail pill**

Find the component that renders a selected agent's header/detail (e.g. `AgentTab.tsx` or `CockpitTab.tsx` — grep for where `session.status`/`BusyIdleBadge` is shown in detail). Add, next to the status badge, when supervised:
```tsx
{session.supervised && <span className="supervised-pill">supervised</span>}
```
(Use the actual session variable name in that component.)

- [ ] **Step 4: Build + test**

Run: `go build ./... && go test ./internal/tui/ ./internal/cli/ -v` and (from `web/`) `npm run build`.
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/sessions.go internal/tui/detail.go web/src/components/
git commit -m "feat: surface supervised mode in cli status, tui + web detail"
```

---

## Task 9: docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document supervised mode**

In `README.md`, under the `start` command reference and/or the approvals-inbox/`AGENTCTL_APPROVALS` section, add a short note:
- `agentctl start … --supervised` (and MCP `supervised:true`, web checkbox) launches the agent with `--permission-mode acceptEdits` instead of `--dangerously-skip-permissions`: file edits auto-approve, but other tools (bash writes, network) present the numbered permission prompt — which the approvals inbox surfaces and lets you answer. Default is unsupervised (fully autonomous). Restored agents keep their supervised setting.

- [ ] **Step 2: Full verification**

Run:
```bash
go build ./... && go test ./... && (cd web && npm test && npm run build)
```
Expected: all green; `web/dist` rebuilt; nothing tracked under `web/dist` except `.gitkeep`.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document supervised spawn mode"
```

- [ ] **Step 4: Manual smoke (left to user)**

Rebuild+reinstall the daemon (`make reinstall`), then spawn a supervised agent (`agentctl start "edit a file then run a git command" --supervised`, or the web checkbox) and confirm: an edit auto-approves, a bash command presents the numbered menu, and that prompt appears as **recognized** with answer buttons/keys in the approvals inbox (web AttentionQueue + TUI ⏳ row). This is also the first chance to verify the parser regex against a REAL permission box (the outstanding fixtures-vs-live gap).

---

## Verification Checklist

- [ ] Default spawn unchanged (`--dangerously-skip-permissions`); `Supervised:true` → `--permission-mode acceptEdits`.
- [ ] `Supervised` persisted on the session and reused by `Restore`.
- [ ] Threaded through client → daemon → adapter → lifecycle, and exposed at CLI (`--supervised`), MCP (`supervised`), web (checkbox).
- [ ] Supervised state visible in CLI `status`, TUI detail, web detail.
- [ ] `go build ./...`, `go test ./...`, `cd web && npm test && npm run build` all pass.
