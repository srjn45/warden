# Approvals Inbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user clear routine agent tool-permission prompts from one place (web inbox + TUI pinned row) without attaching to each tmux session, degrading safely to "attach to answer" for anything not recognized — all behind a feature toggle.

**Architecture:** A daemon-owned approval engine (`internal/approval`) parses Claude Code's numbered permission prompt out of a tmux pane capture and fingerprints its options. Two HTTP endpoints expose it: `GET /approvals` (the live queue, built from each waiting agent's stored pane excerpt) and `POST /sessions/{id}/approve` (re-captures the pane fresh, re-parses, verifies the fingerprint still matches, then injects the chosen digit via `tmux send-keys`). The web UI and TUI are thin renderers of `GET /approvals`; existing SSE drives live updates. Everything is gated behind `AGENTCTL_APPROVALS` (off by default).

**Tech Stack:** Go (chi router, table-driven tests with testify), TypeScript/Preact (Astro web UI, vitest), Bubble Tea (TUI).

**Spec:** `docs/superpowers/specs/2026-06-03-approvals-inbox-design.md`

---

## File Structure

**New files:**
- `internal/approval/approval.go` — `Parse`, `Fingerprint`, `BuildView`, types `Approval` + `View`. Pure, no I/O.
- `internal/approval/approval_test.go` — table-driven parse tests + fixtures.
- `internal/approval/testdata/*.txt` — captured-pane fixtures (recognized + negatives).
- `internal/daemon/approvals_routes.go` — `handleApprovals`, `handleApprove`, request/response DTOs.
- `internal/daemon/approvals_routes_test.go` — endpoint tests.
- `internal/tui/approvals.go` — `renderApprovalsQueue` + helpers.
- `internal/tui/approvals_test.go` — render + row tests.

**Modified files:**
- `internal/config/config.go` — add `ApprovalsEnabled` + `approvalsEnabled()`.
- `internal/lifecycle/lifecycle.go` — add `SendKeys`.
- `internal/daemon/api.go` — `Server.approvals` field; `Lifecycle` interface gains `SendKeys`; register routes.
- `internal/daemon/server.go` — `NewServer` gains `approvals bool` param.
- `internal/daemon/lifecycle_adapter.go` — adapter `SendKeys`.
- `internal/cli/daemon.go` — pass `cfg.ApprovalsEnabled` into `NewServer`.
- `internal/client/client.go` — `Approvals`, `Approve` client methods; import `internal/approval`.
- `web/src/lib/types.ts` — `ApprovalView` type.
- `web/src/lib/api.ts` — `listApprovals`, `approve`.
- `web/src/components/AttentionQueue.tsx` — option buttons / attach fallback.
- `internal/tui/list.go` — synthetic approvals row in `buildItems` consumers.
- `internal/tui/model.go` — approvals state, `items()`, focus handling.
- `internal/tui/keys.go` — `tab` into inbox; (focused handling lives in `model.go` Update).
- `internal/tui/cmds.go` — `approvalsCmd`, `approveCmd`; extend `api` interface.
- `internal/tui/view.go` — render queue in detail pane when inbox selected.
- `README.md` — document `AGENTCTL_APPROVALS`.

---

## Task 1: Config toggle

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestApprovalsEnabled(t *testing.T) {
	t.Setenv("AGENTCTL_APPROVALS", "on")
	require.True(t, Load().ApprovalsEnabled)

	t.Setenv("AGENTCTL_APPROVALS", "")
	require.False(t, Load().ApprovalsEnabled)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestApprovalsEnabled -v`
Expected: FAIL — `ApprovalsEnabled` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the field to `Config`:

```go
type Config struct {
	Addr              string
	DataDir           string
	ClaudeProjectsDir string
	NotifyEnabled     bool
	ApprovalsEnabled  bool
}
```

Add the reader after `notifyEnabled`:

```go
// approvalsEnabled reads AGENTCTL_APPROVALS; off by default, on only for
// 1/on/true. Gates the approvals-inbox feature (parse + inline answer).
func approvalsEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_APPROVALS")) {
	case "1", "on", "true":
		return true
	}
	return false
}
```

Set it in `Load()`:

```go
		NotifyEnabled:     notifyEnabled(),
		ApprovalsEnabled:  approvalsEnabled(),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestApprovalsEnabled -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): AGENTCTL_APPROVALS toggle (off by default)"
```

---

## Task 2: Capture a real prompt fixture

The exact byte layout of Claude Code's permission box (box-drawing chars, the `❯` cursor, indentation) must come from ground truth, not a guess. Capture it first; the parser is written against it.

**Files:**
- Create: `internal/approval/testdata/bash_prompt.txt`
- Create: `internal/approval/testdata/edit_prompt.txt`
- Create: `internal/approval/testdata/freeform.txt`
- Create: `internal/approval/testdata/no_box.txt`

- [ ] **Step 1: Spawn a throwaway agent and capture a live permission prompt**

```bash
# With the daemon running, spawn an agent that will hit a Bash permission prompt,
# then capture its pane once it blocks (replace <tmux> with the session name from `agentctl ls`):
tmux capture-pane -p -t <tmux> -S -200 > internal/approval/testdata/bash_prompt.txt
```

Repeat for an Edit/Write permission prompt → `edit_prompt.txt`.

- [ ] **Step 2: Capture negatives**

- `freeform.txt`: a pane where Claude asks an open question (no numbered option box) — e.g. plan-mode "Would you like to proceed?" with the plan text, or any `❯`-free state.
- `no_box.txt`: a pane of ordinary working output (no prompt at all). Save the last 200 lines of any `working` agent.

- [ ] **Step 3: Eyeball the fixtures**

Open `bash_prompt.txt` and confirm the numbered options are present and note the leading characters before `1.` (spaces, `│`, `❯`). The Task 3 regex must match these exactly.

- [ ] **Step 4: Commit**

```bash
git add internal/approval/testdata/
git commit -m "test(approval): captured-pane fixtures for parser"
```

---

## Task 3: `approval.Parse`

**Files:**
- Create: `internal/approval/approval.go`
- Test: `internal/approval/approval_test.go`

- [ ] **Step 1: Write the failing test**

`internal/approval/approval_test.go`:

```go
package approval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func TestParseRecognizesBashPrompt(t *testing.T) {
	a, ok := Parse(readFixture(t, "bash_prompt.txt"))
	require.True(t, ok)
	require.GreaterOrEqual(t, len(a.Options), 2)
	require.NotEmpty(t, a.Question)
}

func TestParseRejectsFreeform(t *testing.T) {
	_, ok := Parse(readFixture(t, "freeform.txt"))
	require.False(t, ok)
}

func TestParseRejectsNoBox(t *testing.T) {
	_, ok := Parse(readFixture(t, "no_box.txt"))
	require.False(t, ok)
}

func TestParseRejectsSingleOption(t *testing.T) {
	// One numbered line is not a yes/no choice — not answerable inline.
	_, ok := Parse("Something\n  1. Only choice\n")
	require.False(t, ok)
}

func TestParseRejectsNonSequential(t *testing.T) {
	_, ok := Parse("Do you want to proceed?\n  1. Yes\n  3. No\n")
	require.False(t, ok)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/approval/ -run TestParse -v`
Expected: FAIL — package/`Parse` undefined.

- [ ] **Step 3: Implement `Parse`**

`internal/approval/approval.go`:

```go
// Package approval parses Claude Code's numbered tool-permission prompt out of a
// tmux pane capture and fingerprints its options, so the daemon can offer
// one-click answers for recognized prompts and route everything else to attach.
package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Approval is a recognized numbered tool-permission prompt.
type Approval struct {
	Action      string   // e.g. "Bash(rm -rf node_modules)"; "" if not found
	Question    string   // e.g. "Do you want to proceed?"; "" if not found
	Options     []string // option labels, top-down, 1-indexed by position
	SelectedIdx int      // 1-based index of the ❯-highlighted option; 0 if none
}

// optionRe matches a numbered option line, tolerating leading box-drawing
// characters and an optional ❯ selection cursor. Verify against the captured
// fixtures (Task 2) and widen the leading class if the real capture differs.
var optionRe = regexp.MustCompile(`^[\s│┃|]*(❯?)\s*(\d+)\.\s+(.+?)\s*$`)

// boxTrim strips leading/trailing box-drawing chrome from a non-option line.
var boxTrim = strings.NewReplacer("│", " ", "┃", " ", "|", " ", "╭", " ",
	"╮", " ", "╰", " ", "╯", " ", "─", " ")

// Parse recognizes the prompt only on a confident match: a contiguous run of
// options numbered 1..N (N>=2) at the bottom of the pane. Freeform prompts,
// multi-selects, text-entry fields, and partial redraws return ok=false.
func Parse(pane string) (Approval, bool) {
	lines := strings.Split(pane, "\n")

	// The live prompt sits at the bottom: find the last option line.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if optionRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return Approval{}, false
	}

	// Walk up while lines stay options; collect labels + the selected index.
	start := end
	for start-1 >= 0 && optionRe.MatchString(lines[start-1]) {
		start--
	}

	var opts []string
	sel := 0
	for i := start; i <= end; i++ {
		m := optionRe.FindStringSubmatch(lines[i])
		// Numbering must be sequential 1..N.
		if m[2] != strconv.Itoa(i-start+1) {
			return Approval{}, false
		}
		if m[1] == "❯" {
			sel = i - start + 1
		}
		opts = append(opts, strings.TrimSpace(m[3]))
	}
	if len(opts) < 2 {
		return Approval{}, false
	}

	a := Approval{Options: opts, SelectedIdx: sel}

	// Question = nearest non-empty line above the run; Action = the next
	// non-empty line above that, if it looks like a Tool(...) call.
	i := start - 1
	for ; i >= 0; i-- {
		if t := strings.TrimSpace(boxTrim.Replace(lines[i])); t != "" {
			a.Question = t
			i--
			break
		}
	}
	for ; i >= 0; i-- {
		t := strings.TrimSpace(boxTrim.Replace(lines[i]))
		if t == "" {
			continue
		}
		if looksLikeAction(t) {
			a.Action = t
		}
		break
	}
	return a, true
}

// looksLikeAction reports whether a line resembles a tool invocation header
// such as "Bash(...)" or "Edit(path)".
func looksLikeAction(s string) bool {
	open := strings.IndexByte(s, '(')
	return open > 0 && strings.HasSuffix(s, ")")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/approval/ -run TestParse -v`
Expected: PASS. If `TestParseRecognizesBashPrompt` fails, adjust `optionRe`'s leading character class to match the real fixture (Step 3 of Task 2 noted what precedes `1.`), then re-run.

- [ ] **Step 5: Commit**

```bash
git add internal/approval/approval.go internal/approval/approval_test.go
git commit -m "feat(approval): Parse recognizes numbered permission prompts"
```

---

## Task 4: `Fingerprint` + `BuildView`

**Files:**
- Modify: `internal/approval/approval.go`
- Test: `internal/approval/approval_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/approval/approval_test.go`:

```go
func TestFingerprintStableAndDistinct(t *testing.T) {
	a := Fingerprint([]string{"Yes", "No"})
	require.Equal(t, a, Fingerprint([]string{"Yes", "No"})) // stable
	require.NotEqual(t, a, Fingerprint([]string{"Yes", "Maybe"}))
	require.NotEmpty(t, a)
}

func TestBuildViewRecognized(t *testing.T) {
	v := BuildView("agent-1", readFixture(t, "bash_prompt.txt"))
	require.Equal(t, "agent-1", v.ID)
	require.True(t, v.Recognized)
	require.NotEmpty(t, v.Fingerprint)
	require.GreaterOrEqual(t, len(v.Options), 2)
}

func TestBuildViewUnrecognized(t *testing.T) {
	v := BuildView("agent-2", readFixture(t, "freeform.txt"))
	require.Equal(t, "agent-2", v.ID)
	require.False(t, v.Recognized)
	require.Empty(t, v.Options)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/approval/ -run 'TestFingerprint|TestBuildView' -v`
Expected: FAIL — `Fingerprint`/`BuildView`/`View` undefined.

- [ ] **Step 3: Implement**

Append to `internal/approval/approval.go`:

```go
// View is the wire shape returned by GET /approvals and consumed by both UIs.
type View struct {
	ID          string   `json:"id"`
	Action      string   `json:"action"`
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Fingerprint string   `json:"fingerprint"`
	Recognized  bool     `json:"recognized"`
}

// Fingerprint is a stable short hash of the option labels. The UI echoes it back
// on answer so the daemon can prove the prompt has not changed underneath it.
func Fingerprint(opts []string) string {
	h := sha256.Sum256([]byte(strings.Join(opts, "\x00")))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// BuildView parses pane for session id, returning a recognized View with options
// + fingerprint, or an unrecognized View (Recognized=false) to route to attach.
func BuildView(id, pane string) View {
	a, ok := Parse(pane)
	if !ok {
		return View{ID: id, Recognized: false}
	}
	return View{
		ID: id, Action: a.Action, Question: a.Question,
		Options: a.Options, Fingerprint: Fingerprint(a.Options), Recognized: true,
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/approval/ -v`
Expected: PASS (all approval tests).

- [ ] **Step 5: Commit**

```bash
git add internal/approval/approval.go internal/approval/approval_test.go
git commit -m "feat(approval): Fingerprint + BuildView wire shape"
```

---

## Task 5: `lifecycle.SendKeys` + interface + adapter

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Modify: `internal/daemon/api.go` (Lifecycle interface)
- Modify: `internal/daemon/lifecycle_adapter.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go` (mirror the existing `TestInput*` tests' fake-runner setup — reuse whatever recording runner those tests use; this asserts the exact tmux argv):

```go
func TestSendKeysSendsRawDigit(t *testing.T) {
	rr := &recordRunner{} // same fake runner type used by TestInputBracketPastesThenSubmits
	l := &Lifecycle{run: rr}
	require.NoError(t, l.SendKeys(context.Background(), "sess-1", "2"))
	require.Equal(t,
		[]string{"tmux", "send-keys", "-t", "sess-1", "2"},
		rr.lastArgs(), // adapt to the recorder's accessor
	)
}
```

(If the existing input tests use a different recorder shape, copy that shape exactly — the point is to assert the argv is `send-keys -t <sess> <key>` with no Enter and no paste-buffer.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSendKeys -v`
Expected: FAIL — `SendKeys` undefined.

- [ ] **Step 3: Implement `SendKeys`**

In `internal/lifecycle/lifecycle.go`, after `Input`:

```go
// SendKeys sends a single key (e.g. a numbered menu choice) to the agent's tmux
// pane as a raw keystroke. Unlike Input it neither bracketed-pastes nor appends
// Enter: Claude Code's select prompts treat the digit itself as select-and-
// confirm, so an extra Enter could double-submit.
func (l *Lifecycle) SendKeys(ctx context.Context, tmuxSession, key string) error {
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", tmuxSession, key); err != nil {
		return fmt.Errorf("tmux send-keys %q: %w: %s", key, err, out)
	}
	return nil
}
```

- [ ] **Step 4: Add to the daemon `Lifecycle` interface**

In `internal/daemon/api.go`, inside `type Lifecycle interface`, after `Output(...)`:

```go
	// SendKeys injects a raw keystroke (e.g. a menu digit) into the agent's pane.
	SendKeys(ctx context.Context, tmuxSession, key string) error
```

- [ ] **Step 5: Implement the adapter method**

In `internal/daemon/lifecycle_adapter.go`, after `Output`:

```go
func (a *lifecycleAdapter) SendKeys(ctx context.Context, tmuxSession, key string) error {
	return a.lc.SendKeys(ctx, tmuxSession, key)
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/ -run TestSendKeys -v && go build ./...`
Expected: PASS and a clean build. (The `fakeLife` in `internal/daemon` is extended in Task 7, so daemon tests may not compile until then — that's fine; build the lifecycle package now.)

- [ ] **Step 7: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go internal/daemon/api.go internal/daemon/lifecycle_adapter.go
git commit -m "feat(lifecycle): SendKeys for raw keystroke injection"
```

---

## Task 6: `GET /approvals` endpoint

**Files:**
- Create: `internal/daemon/approvals_routes.go`
- Modify: `internal/daemon/api.go` (Server field + route registration)
- Modify: `internal/daemon/server.go` (NewServer param)
- Modify: `internal/cli/daemon.go` (pass the toggle)
- Test: `internal/daemon/approvals_routes_test.go`

- [ ] **Step 1: Add the `approvals` field + route + NewServer param**

In `internal/daemon/api.go`, add to the `Server` struct (after `done`):

```go
	// approvals gates the approvals-inbox endpoints (AGENTCTL_APPROVALS).
	approvals bool
```

In `router()`, register the routes (before `s.registerStatic(r)`):

```go
	r.Get("/approvals", s.handleApprovals)
	r.Post("/sessions/{id}/approve", s.handleApprove)
```

In `internal/daemon/server.go`, change `NewServer`:

```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, approvals bool) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{
		store: st, life: life, poller: p, pollInterval: interval,
		hub: h, done: make(chan struct{}), approvals: approvals,
	}
}
```

In `internal/cli/daemon.go`, update the construction:

```go
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled)
```

- [ ] **Step 2: Write the failing test**

`internal/daemon/approvals_routes_test.go`. The existing `fakeLife` (in `lifecycle_routes_test.go`) must gain a `SendKeys` method and a settable `output` — add the method there in this step:

```go
// add to fakeLife in lifecycle_routes_test.go:
func (f *fakeLife) SendKeys(_ context.Context, s, key string) error { f.lastKey = key; return nil }
// and add fields: lastKey string  (output string already exists)
```

Then the test file:

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func approvalsServer(t *testing.T, fs *fakeStore, fl *fakeLife, on bool) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs, life: fl, approvals: on}
	return httptest.NewServer(srv.router())
}

func TestGetApprovalsDisabled(t *testing.T) {
	ts := approvalsServer(t, newFakeStore(), &fakeLife{}, false)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/approvals")
	require.NoError(t, err)
	var out approvalsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.False(t, out.Enabled)
	require.Empty(t, out.Approvals)
}

func TestGetApprovalsListsWaiting(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{
		ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput,
		LastPaneExcerpt: "Do you want to proceed?\n  1. Yes\n  2. No\n",
	}
	fs.data["a2"] = &store.Session{ID: "a2", Status: store.StatusWorking}
	ts := approvalsServer(t, fs, &fakeLife{}, true)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/approvals")
	require.NoError(t, err)
	var out approvalsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.True(t, out.Enabled)
	require.Len(t, out.Approvals, 1) // only the waiting agent
	require.Equal(t, "a1", out.Approvals[0].ID)
	require.True(t, out.Approvals[0].Recognized)
}
```

(Task 7 adds the `bytes` and `internal/approval` imports to this file when the POST tests need them.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestGetApprovals -v`
Expected: FAIL — `handleApprovals`/`approvalsResponse` undefined.

- [ ] **Step 4: Implement the handler**

`internal/daemon/approvals_routes.go`:

```go
package daemon

import (
	"net/http"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/store"
)

// approvalsResponse is the body for GET /approvals.
type approvalsResponse struct {
	Enabled   bool            `json:"enabled"`
	Approvals []approval.View `json:"approvals"`
}

// handleApprovals returns the live queue: every waiting_for_input session parsed
// from its stored pane excerpt (recognized options or the unrecognized flag).
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if !s.approvals {
		writeJSON(w, http.StatusOK, approvalsResponse{Enabled: false, Approvals: []approval.View{}})
		return
	}
	sessions, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := []approval.View{}
	for _, sess := range sessions {
		if sess.Status != store.StatusWaitingForInput {
			continue
		}
		views = append(views, approval.BuildView(sess.ID, sess.LastPaneExcerpt))
	}
	writeJSON(w, http.StatusOK, approvalsResponse{Enabled: true, Approvals: views})
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestGetApprovals -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/approvals_routes.go internal/daemon/api.go internal/daemon/server.go internal/cli/daemon.go internal/daemon/approvals_routes_test.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): GET /approvals live queue behind toggle"
```

---

## Task 7: `POST /sessions/{id}/approve` with re-verify guard

**Files:**
- Modify: `internal/daemon/approvals_routes.go`
- Test: `internal/daemon/approvals_routes_test.go`

- [ ] **Step 1: Write the failing tests**

Add the imports `"bytes"` and `"github.com/srajanpathak/agentctl/internal/approval"` to `internal/daemon/approvals_routes_test.go`'s import block (the POST tests need them), then add:

```go
func TestPostApproveHappyPath(t *testing.T) {
	pane := "Do you want to proceed?\n  1. Yes\n  2. No\n"
	fp := approval.Fingerprint([]string{"Yes", "No"})
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	fl := &fakeLife{output: pane}
	ts := approvalsServer(t, fs, fl, true)
	defer ts.Close()

	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: fp})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "1", fl.lastKey) // injected the digit
}

func TestPostApproveStaleFingerprint(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	fl := &fakeLife{output: "Do you want to proceed?\n  1. Yes\n  2. No\n"}
	ts := approvalsServer(t, fs, fl, true)
	defer ts.Close()

	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: "deadbeef"})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, fl.lastKey) // never injected
}

func TestPostApproveDisabled(t *testing.T) {
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1"}
	ts := approvalsServer(t, fs, &fakeLife{}, false)
	defer ts.Close()
	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: "x"})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPostApproveOutOfRange(t *testing.T) {
	pane := "Do you want to proceed?\n  1. Yes\n  2. No\n"
	fp := approval.Fingerprint([]string{"Yes", "No"})
	fs := newFakeStore()
	fs.data["a1"] = &store.Session{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	ts := approvalsServer(t, fs, &fakeLife{output: pane}, true)
	defer ts.Close()
	body, _ := json.Marshal(ApproveRequest{Option: 9, Fingerprint: fp})
	resp, err := http.Post(ts.URL+"/sessions/a1/approve", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestPostApprove -v`
Expected: FAIL — `handleApprove`/`ApproveRequest` undefined.

- [ ] **Step 3: Implement the handler**

Append to `internal/daemon/approvals_routes.go` (add imports `encoding/json`, `errors`, `strconv`, `github.com/go-chi/chi/v5`):

```go
// ApproveRequest is the body for POST /sessions/{id}/approve.
type ApproveRequest struct {
	Option      int    `json:"option"`      // 1-based choice
	Fingerprint string `json:"fingerprint"` // the options hash the UI rendered
}

// handleApprove answers a recognized prompt with a re-verify guard: it
// re-captures the pane fresh, re-parses, and injects the digit ONLY if the
// fingerprint still matches — otherwise 409, so we never answer a prompt that
// changed underneath the user.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !s.approvals {
		writeErr(w, http.StatusForbidden, "approvals disabled")
		return
	}
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	pane, err := s.life.Output(r.Context(), sess.TmuxSession, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a, ok := approval.Parse(pane)
	if !ok || approval.Fingerprint(a.Options) != req.Fingerprint {
		writeErr(w, http.StatusConflict, "prompt changed; reopen")
		return
	}
	if req.Option < 1 || req.Option > len(a.Options) {
		writeErr(w, http.StatusBadRequest, "option out of range")
		return
	}
	if err := s.life.SendKeys(r.Context(), sess.TmuxSession, strconv.Itoa(req.Option)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "answered"})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestPostApprove -v`
Expected: PASS.

- [ ] **Step 5: Full daemon + build check**

Run: `go test ./... && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/approvals_routes.go internal/daemon/approvals_routes_test.go
git commit -m "feat(daemon): POST /approve with re-verify fingerprint guard"
```

---

## Task 8: Client methods (`Approvals`, `Approve`)

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go` (mirror existing client tests if present; otherwise create)

- [ ] **Step 1: Write the failing test**

Add to `internal/client/client_test.go` (use the package's existing httptest helper pattern; the sketch below stands alone):

```go
func TestClientApprovals(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/approvals", r.URL.Path)
		w.Write([]byte(`{"enabled":true,"approvals":[{"id":"a1","recognized":true,"options":["Yes","No"],"fingerprint":"ff"}]}`))
	}))
	defer ts.Close()
	c := New(ts.URL)
	enabled, views, err := c.Approvals(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	require.Len(t, views, 1)
	require.Equal(t, "a1", views[0].ID)
}

func TestClientApprove(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/sessions/a1/approve", r.URL.Path)
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"status":"answered"}`))
	}))
	defer ts.Close()
	c := New(ts.URL)
	require.NoError(t, c.Approve(context.Background(), "a1", 2, "ff"))
	require.Equal(t, float64(2), gotBody["option"])
	require.Equal(t, "ff", gotBody["fingerprint"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run 'TestClientApprov' -v`
Expected: FAIL — `Approvals`/`Approve` undefined.

- [ ] **Step 3: Implement**

In `internal/client/client.go`, add the import `"github.com/srajanpathak/agentctl/internal/approval"` and, after `Input`:

```go
// Approvals fetches the live approval queue. Returns (enabled, views, err);
// enabled is false when the daemon has the feature toggled off.
func (c *Client) Approvals(ctx context.Context) (bool, []approval.View, error) {
	var resp struct {
		Enabled   bool            `json:"enabled"`
		Approvals []approval.View `json:"approvals"`
	}
	if err := c.do(ctx, http.MethodGet, "/approvals", nil, &resp); err != nil {
		return false, nil, err
	}
	return resp.Enabled, resp.Approvals, nil
}

// Approve answers a recognized prompt with the 1-based option and the options
// fingerprint the UI rendered (for the daemon's re-verify guard).
func (c *Client) Approve(ctx context.Context, id string, option int, fingerprint string) error {
	body := map[string]any{"option": option, "fingerprint": fingerprint}
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/approve", body, nil)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run 'TestClientApprov' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): Approvals + Approve methods"
```

---

## Task 9: Web — types + api client

**Files:**
- Modify: `web/src/lib/types.ts`
- Modify: `web/src/lib/api.ts`
- Test: `web/src/lib/api.test.ts`

- [ ] **Step 1: Write the failing test**

Add to `web/src/lib/api.test.ts` (follow the file's existing fetch-mock style):

```ts
import { listApprovals, approve } from './api';

it('listApprovals returns the queue', async () => {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ enabled: true, approvals: [{ id: 'a1', recognized: true, options: ['Yes', 'No'], fingerprint: 'ff' }] }),
  }) as any;
  const r = await listApprovals();
  expect(r.enabled).toBe(true);
  expect(r.approvals[0].id).toBe('a1');
});

it('approve posts option + fingerprint', async () => {
  const f = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ status: 'answered' }) });
  globalThis.fetch = f as any;
  await approve('a1', 2, 'ff');
  expect(f).toHaveBeenCalledWith('/sessions/a1/approve', expect.objectContaining({ method: 'POST' }));
  const body = JSON.parse((f.mock.calls[0][1] as any).body);
  expect(body).toEqual({ option: 2, fingerprint: 'ff' });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run (from `web/`): `npm test -- api.test`
Expected: FAIL — `listApprovals`/`approve` not exported.

- [ ] **Step 3: Implement**

Add to `web/src/lib/types.ts`:

```ts
export interface ApprovalView {
  id: string;
  action: string;
  question: string;
  options: string[];
  fingerprint: string;
  recognized: boolean;
}
```

Add to `web/src/lib/api.ts` (import the type):

```ts
import type { Session, ApprovalView } from './types';

export async function listApprovals(): Promise<{ enabled: boolean; approvals: ApprovalView[] }> {
  const data = await parse<{ enabled: boolean; approvals: ApprovalView[] | null }>(await fetch('/approvals'));
  return { enabled: data.enabled, approvals: data.approvals ?? [] };
}

export async function approve(id: string, option: number, fingerprint: string): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/approve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ option, fingerprint }),
  }));
}
```

(Update the existing `import type { Session } from './types';` line to also import `ApprovalView`.)

- [ ] **Step 4: Run test to verify it passes**

Run (from `web/`): `npm test -- api.test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/api.ts web/src/lib/api.test.ts
git commit -m "feat(web): listApprovals + approve api client"
```

---

## Task 10: Web — answerable AttentionQueue

**Files:**
- Modify: `web/src/components/AttentionQueue.tsx`
- Test: `web/src/components/AttentionQueue.test.tsx` (create if the components dir has no test; otherwise add)

The queue currently maps `needsAttention(sessions)` to click-to-pin cards. We layer approval option buttons on top: the component fetches `GET /approvals` and, for a recognized waiting agent, renders one button per option (clicking calls `approve`); unrecognized waiting agents and errored/orphaned agents keep the click-to-attach card.

- [ ] **Step 1: Write the failing test**

`web/src/components/AttentionQueue.test.tsx`:

```tsx
import { render, screen, fireEvent, waitFor } from '@testing-library/preact';
import AttentionQueue from './AttentionQueue';
import * as api from '../lib/api';

const waiting = { id: 'a1', status: 'waiting_for_input' } as any;

it('renders option buttons for a recognized prompt and answers on click', async () => {
  vi.spyOn(api, 'listApprovals').mockResolvedValue({
    enabled: true,
    approvals: [{ id: 'a1', action: 'Bash(ls)', question: 'Do you want to proceed?', options: ['Yes', 'No'], fingerprint: 'ff', recognized: true }],
  });
  const approveSpy = vi.spyOn(api, 'approve').mockResolvedValue();
  render(<AttentionQueue sessions={[waiting]} onSelect={() => {}} />);
  const yes = await screen.findByRole('button', { name: /1\. Yes/ });
  fireEvent.click(yes);
  await waitFor(() => expect(approveSpy).toHaveBeenCalledWith('a1', 1, 'ff'));
});

it('falls back to attach for an unrecognized prompt', async () => {
  vi.spyOn(api, 'listApprovals').mockResolvedValue({
    enabled: true,
    approvals: [{ id: 'a1', action: '', question: '', options: [], fingerprint: '', recognized: false }],
  });
  const onSelect = vi.fn();
  render(<AttentionQueue sessions={[waiting]} onSelect={onSelect} />);
  const card = await screen.findByText(/attach to answer/i);
  fireEvent.click(card);
  expect(onSelect).toHaveBeenCalledWith('a1');
});
```

- [ ] **Step 2: Run test to verify it fails**

Run (from `web/`): `npm test -- AttentionQueue`
Expected: FAIL — no option buttons rendered.

- [ ] **Step 3: Implement**

Rewrite `web/src/components/AttentionQueue.tsx`:

```tsx
import { useEffect, useState } from 'preact/hooks';
import type { Session, ApprovalView } from '../lib/types';
import { needsAttention } from '../lib/attention';
import { listApprovals, approve } from '../lib/api';
import BusyIdleBadge from './BusyIdleBadge';

// AttentionQueue surfaces agents blocked on the user or failed. For recognized
// permission prompts it renders one-click option buttons (answer without
// attaching); unrecognized prompts and failures fall back to click-to-attach.
export default function AttentionQueue({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  const items = needsAttention(sessions);
  const [byId, setById] = useState<Record<string, ApprovalView>>({});

  // Refetch the parsed queue whenever the set of waiting agents changes.
  const waitingKey = items.filter((s) => s.status === 'waiting_for_input').map((s) => s.id).join(',');
  useEffect(() => {
    let live = true;
    listApprovals().then((r) => {
      if (!live) return;
      const m: Record<string, ApprovalView> = {};
      for (const v of r.approvals) m[v.id] = v;
      setById(m);
    }).catch(() => { /* feature off or transient — fall back to cards */ });
    return () => { live = false; };
  }, [waitingKey]);

  if (items.length === 0) {
    return <p className="muted attn-empty">Nothing needs you right now. ✅</p>;
  }

  async function answer(id: string, option: number, fingerprint: string) {
    try {
      await approve(id, option, fingerprint);
    } catch {
      // Prompt changed/disabled — drop the stale buttons; SSE will refresh.
      setById((prev) => { const n = { ...prev }; delete n[id]; return n; });
    }
  }

  return (
    <div className="attn-queue">
      {items.map((s) => {
        const v = byId[s.id];
        const answerable = s.status === 'waiting_for_input' && v && v.recognized;
        return (
          <div key={s.id} className="attn-card">
            <div className="attn-card-head">
              <b>{s.id}</b> <BusyIdleBadge status={s.status} />
            </div>
            <div className="muted attn-card-sub">
              {(v && (v.action || v.question)) || s.subject || s.prompt || s.type || '—'}
            </div>
            {answerable ? (
              <div className="attn-options">
                {v.options.map((label, i) => (
                  <button
                    key={i}
                    className="attn-option"
                    onClick={() => answer(s.id, i + 1, v.fingerprint)}
                  >
                    {i + 1}. {label}
                  </button>
                ))}
              </div>
            ) : (
              <button className="attn-attach" onClick={() => onSelect(s.id)}>
                {s.status === 'waiting_for_input' ? 'attach to answer' : 'open'}
              </button>
            )}
          </div>
        );
      })}
    </div>
  );
}
```

- [ ] **Step 4: Run test to verify it passes**

Run (from `web/`): `npm test -- AttentionQueue`
Expected: PASS.

- [ ] **Step 5: Build the web bundle**

Run (from `web/`): `npm run build`
Expected: clean build (the daemon embeds `web/dist`).

- [ ] **Step 6: Commit**

```bash
git add web/src/components/AttentionQueue.tsx web/src/components/AttentionQueue.test.tsx
git commit -m "feat(web): answerable AttentionQueue with option buttons"
```

---

## Task 11: TUI — data layer (cmds, msgs, api interface, model state)

**Files:**
- Modify: `internal/tui/cmds.go`
- Modify: `internal/tui/model.go`
- Test: `internal/tui/cmds_test.go`

- [ ] **Step 1: Inspect the `api` interface and tick wiring**

Run: `grep -n 'type api interface\|func listCmd\|func tick\|tickMsg\|sessionsMsg' internal/tui/cmds.go`
Note the interface method list and the message types — you will mirror `List`/`listCmd`/`sessionsMsg`.

- [ ] **Step 2: Write the failing test**

Add to `internal/tui/cmds_test.go` (mirror the existing `listCmd` test's fake `api`):

```go
func TestApprovalsCmdEmitsMsg(t *testing.T) {
	fa := &fakeAPI{approvals: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes", "No"}, Fingerprint: "ff"}}, approvalsOn: true}
	msg := approvalsCmd(fa)()
	am, ok := msg.(approvalsMsg)
	require.True(t, ok)
	require.True(t, am.enabled)
	require.Len(t, am.views, 1)
}
```

Extend the test file's `fakeAPI` with these fields and methods (add the `internal/approval` import):

```go
// fields on fakeAPI:
approvals    []approval.View
approvalsOn  bool
approveErr   error
lastApproved struct{ id string; option int; fp string }

func (f *fakeAPI) Approvals(_ context.Context) (bool, []approval.View, error) {
	return f.approvalsOn, f.approvals, nil
}
func (f *fakeAPI) Approve(_ context.Context, id string, option int, fp string) error {
	f.lastApproved = struct{ id string; option int; fp string }{id, option, fp}
	return f.approveErr
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestApprovalsCmd -v`
Expected: FAIL — `approvalsCmd`/`approvalsMsg` undefined.

- [ ] **Step 4: Implement cmds, msgs, and extend the `api` interface**

In `internal/tui/cmds.go`, add `"github.com/srajanpathak/agentctl/internal/approval"` to imports, add the two methods to the `api` interface:

```go
	Approvals(ctx context.Context) (bool, []approval.View, error)
	Approve(ctx context.Context, id string, option int, fingerprint string) error
```

Add the messages and commands (mirror `sessionsMsg`/`listCmd`):

```go
type approvalsMsg struct {
	enabled bool
	views   []approval.View
	err     error
}

type approveResultMsg struct {
	id  string
	err error
}

func approvalsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout) // reuse the timeout const listCmd uses
		defer cancel()
		on, views, err := a.Approvals(ctx)
		return approvalsMsg{enabled: on, views: views, err: err}
	}
}

func approveCmd(a api, id string, option int, fingerprint string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
		defer cancel()
		return approveResultMsg{id: id, err: a.Approve(ctx, id, option, fingerprint)}
	}
}
```

(If the existing commands don't use a `cmdTimeout` const, copy the exact context setup `listCmd` uses.)

Ensure the real `*client.Client` already satisfies the extended interface — it does after Task 8 (`Approvals`/`Approve` exist).

- [ ] **Step 5: Add model state + message handling**

In `internal/tui/model.go`, add fields to `Model`:

```go
	approvals     []approval.View
	apprCursor    int
	apprFocused   bool
	approvalsOn   bool
```

(Add the `internal/approval` import.)

In `Update`, handle the new messages (place beside the `sessionsMsg` case):

```go
	case approvalsMsg:
		if msg.err == nil {
			m.approvalsOn = msg.enabled
			m.approvals = msg.views
			if m.apprCursor >= len(m.approvals) {
				m.apprCursor = max(0, len(m.approvals)-1)
			}
		}
		return m, nil

	case approveResultMsg:
		if msg.err != nil {
			m.status = "answer failed: " + msg.err.Error()
		} else {
			m.status = ""
		}
		return m, approvalsCmd(m.api) // refresh the queue immediately

```

In the tick/refresh batch (the case that currently returns `tea.Batch(listCmd(m.api), outputCmd(...), tick())`), add `approvalsCmd(m.api)`:

```go
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selectedID()), approvalsCmd(m.api), tick())
```

Add a helper near `selected()`:

```go
// curApproval returns the queue entry under the inbox sub-cursor, or nil.
func (m Model) curApproval() *approval.View {
	if m.apprCursor < 0 || m.apprCursor >= len(m.approvals) {
		return nil
	}
	return &m.approvals[m.apprCursor]
}
```

- [ ] **Step 6: Run test + build**

Run: `go test ./internal/tui/ -run TestApprovalsCmd -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/cmds.go internal/tui/model.go internal/tui/cmds_test.go
git commit -m "feat(tui): approvals data layer (cmds, msgs, model state)"
```

---

## Task 12: TUI — synthetic inbox row in the list

**Files:**
- Modify: `internal/tui/list.go`
- Modify: `internal/tui/model.go` (`items()`)
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/list_test.go`:

```go
func TestBuildRowsIncludesApprovalsRow(t *testing.T) {
	items := []item{
		{approvals: true, apprCount: 2},
		{session: &store.Session{ID: "a1"}, dir: "/repo"},
	}
	rows := buildRows(items)
	// Row 0 is the inbox body row (no header), then the group header, then a1.
	require.Equal(t, "", rows[0].header)
	require.Equal(t, 0, rows[0].idx)
	require.NotEqual(t, "", rows[1].header) // /repo (1) header for the agent group
}

func TestItemKeyApprovals(t *testing.T) {
	require.Equal(t, "approvals\x00", itemKey(item{approvals: true}))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestBuildRowsIncludesApprovals|TestItemKeyApprovals' -v`
Expected: FAIL — `approvals`/`apprCount` fields and the row handling don't exist.

- [ ] **Step 3: Implement**

In `internal/tui/list.go`, extend `item`:

```go
type item struct {
	session   *store.Session
	dir       string
	approvals bool // synthetic top-of-list inbox row
	apprCount int  // number of waiting agents (inbox row only)
}
```

Update `itemKey` (handle the inbox row first):

```go
func itemKey(it item) string {
	if it.approvals {
		return "approvals\x00"
	}
	if it.session != nil {
		return it.session.ID
	}
	return dirKey(it.dir)
}
```

Update `buildRows` to emit the inbox row with no group header (replace the `i == 0` first-header logic with a `started` flag so the inbox row doesn't suppress the first real group header):

```go
func buildRows(items []item) []listRow {
	var rows []listRow
	prev := ""
	started := false
	for i := range items {
		if items[i].approvals {
			rows = append(rows, listRow{idx: i}) // no header for the inbox row
			continue
		}
		dir := items[i].dir
		if !started || dir != prev {
			count := 0
			for j := i; j < len(items) && !items[j].approvals && items[j].dir == dir; j++ {
				if items[j].session != nil {
					count++
				}
			}
			rows = append(rows, listRow{header: fmt.Sprintf("%s (%d)", abbrevHome(dir), count)})
			prev = dir
			started = true
		}
		rows = append(rows, listRow{idx: i})
	}
	return rows
}
```

Update `renderItemLine` to render the inbox row (add `strconv` import):

```go
func renderItemLine(it item, selected bool, width int) string {
	var line string
	switch {
	case it.approvals:
		txt := "⏳ Approvals (" + strconv.Itoa(it.apprCount) + ")"
		if it.apprCount == 0 {
			line = stMuted.Render(txt)
		} else {
			line = stStatus.Render(txt)
		}
	case it.session == nil:
		line = stMuted.Render("(no agents — n to spawn here)")
	default:
		s := it.session
		label, st := badge(s.Status)
		line = fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
			trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
			trunc(s.Subject, max(0, width-44)))
	}
	cur := "  "
	if selected {
		cur = stCursor.Render("› ")
		if it.session != nil || it.approvals {
			line = stCursor.Render(line)
		}
	}
	return cur + line
}
```

In `internal/tui/model.go`, prepend the inbox row in `items()` when the feature is on:

```go
func (m Model) items() []item {
	base := buildItems(m.sessions, m.openedDirs)
	if !m.approvalsOn {
		return base
	}
	row := item{approvals: true, apprCount: len(m.approvals)}
	return append([]item{row}, base...)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestBuildRows|TestItemKey|TestRenderList|TestBuildItems' -v`
Expected: PASS (and the existing list tests still pass — the inbox row is only added when `approvalsOn`, which those tests leave false).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/model.go internal/tui/list_test.go
git commit -m "feat(tui): synthetic ⏳ Approvals row pinned at top of list"
```

---

## Task 13: TUI — queue render in the detail pane + focus/answer keys

**Files:**
- Create: `internal/tui/approvals.go`
- Modify: `internal/tui/view.go`
- Modify: `internal/tui/keys.go`
- Modify: `internal/tui/model.go` (focused-key branch in `Update`)
- Test: `internal/tui/approvals_test.go`

- [ ] **Step 1: Write the failing test**

`internal/tui/approvals_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/stretchr/testify/require"
)

func TestRenderApprovalsQueueRecognized(t *testing.T) {
	views := []approval.View{
		{ID: "a1", Action: "Bash(ls)", Question: "Do you want to proceed?", Options: []string{"Yes", "No"}, Recognized: true},
	}
	out := renderApprovalsQueue(views, 0, true, 60, 20)
	require.Contains(t, out, "a1")
	require.Contains(t, out, "Bash(ls)")
	require.Contains(t, out, "[1] Yes")
	require.Contains(t, out, "[2] No")
}

func TestRenderApprovalsQueueUnrecognized(t *testing.T) {
	views := []approval.View{{ID: "a2", Recognized: false}}
	out := renderApprovalsQueue(views, 0, true, 60, 20)
	require.Contains(t, strings.ToLower(out), "attach")
}

func TestRenderApprovalsQueueEmpty(t *testing.T) {
	out := renderApprovalsQueue(nil, 0, false, 60, 20)
	require.Contains(t, strings.ToLower(out), "nothing")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderApprovalsQueue -v`
Expected: FAIL — `renderApprovalsQueue` undefined.

- [ ] **Step 3: Implement the renderer**

`internal/tui/approvals.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/approval"
)

// renderApprovalsQueue renders the waiting-agent queue shown in the detail pane
// when the inbox row is selected. cursor is the sub-cursor index; focused marks
// whether the pane has key focus (drives the caret + hint).
func renderApprovalsQueue(views []approval.View, cursor int, focused bool, width, height int) string {
	if len(views) == 0 {
		return padTo(stMuted.Render("Nothing waiting. ✅"), height)
	}
	hint := "tab to act"
	if focused {
		hint = "↑/↓ move · 1-9 answer · a attach · tab/esc leave"
	}
	var b strings.Builder
	b.WriteString(stMuted.Render(focusHintLabel(hint)) + "\n\n")
	for i, v := range views {
		caret := "  "
		if i == cursor {
			caret = stCursor.Render("› ")
		}
		head := fmt.Sprintf("%s%s", caret, stPaneTitle.Render(v.ID))
		if v.Action != "" {
			head += "  " + stMuted.Render(trunc(v.Action, max(0, width-len(v.ID)-4)))
		}
		b.WriteString(head + "\n")
		if v.Recognized {
			if v.Question != "" {
				b.WriteString("    " + stMuted.Render(trunc(v.Question, max(0, width-4))) + "\n")
			}
			var opts []string
			for j, label := range v.Options {
				opts = append(opts, fmt.Sprintf("[%d] %s", j+1, label))
			}
			b.WriteString("    " + trunc(strings.Join(opts, "  "), max(0, width-4)) + "\n")
		} else {
			b.WriteString("    " + stError.Render("⚠ unrecognized — press a to attach") + "\n")
		}
		b.WriteString("\n")
	}
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}

// focusHintLabel keeps the hint wording in one place.
func focusHintLabel(s string) string { return "approvals — " + s }
```

- [ ] **Step 4: Run renderer test to verify it passes**

Run: `go test ./internal/tui/ -run TestRenderApprovalsQueue -v`
Expected: PASS.

- [ ] **Step 5: Wire the renderer into the detail pane**

In `internal/tui/view.go`, in the `else` branch where `right` is built, branch on the selected item:

```go
		listOuter := m.listOuterW()
		detailOuter := m.w - listOuter
		listTitle := fmt.Sprintf("Agents (%d)", len(m.sessions))

		cur := itemAt(m.items(), m.cursor)
		var detailTitle, detailBody string
		if cur.approvals {
			detailTitle = "Approvals"
			detailBody = renderApprovalsQueue(m.approvals, m.apprCursor, m.apprFocused, detailOuter-2, bodyH-2)
		} else {
			detailTitle = m.selectedID()
			if detailTitle == "" {
				detailTitle = "—"
			}
			detailBody = renderDetail(m.selected(), m.vp, m.outputFocused, detailOuter-2)
		}
		left := titleBox(listTitle, renderList(m.items(), m.cursor, listOuter-2, bodyH-2), listOuter, bodyH)
		right := titleBox(detailTitle, detailBody, detailOuter, bodyH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
```

- [ ] **Step 6: Add the focused-key branch + tab-to-focus**

In `internal/tui/model.go` `Update`, add a branch BEFORE the existing `if m.mode == modeNormal && m.outputFocused {` block (add `strconv` import):

```go
		if m.mode == modeNormal && m.apprFocused {
			switch msg.String() {
			case "tab", "esc":
				m.apprFocused = false
				return m, nil
			case "q", "ctrl+c":
				return m, tea.Quit
			case "down", "j":
				if m.apprCursor < len(m.approvals)-1 {
					m.apprCursor++
				}
				return m, nil
			case "up", "k":
				if m.apprCursor > 0 {
					m.apprCursor--
				}
				return m, nil
			case "a":
				if v := m.curApproval(); v != nil {
					return m, attachCmd(v.ID)
				}
				return m, nil
			case "1", "2", "3", "4", "5", "6", "7", "8", "9":
				v := m.curApproval()
				if v != nil && v.Recognized {
					n, _ := strconv.Atoi(msg.String())
					if n >= 1 && n <= len(v.Options) {
						return m, approveCmd(m.api, v.ID, n, v.Fingerprint)
					}
				}
				return m, nil
			}
			return m, nil
		}
```

In `internal/tui/keys.go`, update the `case "tab":` so it focuses the inbox when the inbox row is selected:

```go
		case "tab":
			if itemAt(m.items(), m.cursor).approvals {
				m.apprFocused = true
			} else if m.selected() != nil {
				m.outputFocused = true
			}
			return m, nil
```

Update the help text in `internal/tui/view.go` `helpText()` to mention the inbox:

```go
		"  i / tab      on the ⏳ Approvals row: focus the queue, answer with 1-9\n" +
```

(Insert after the `tab` line.)

- [ ] **Step 7: Full TUI test + build**

Run: `go test ./internal/tui/ -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/approvals.go internal/tui/approvals_test.go internal/tui/view.go internal/tui/keys.go internal/tui/model.go
git commit -m "feat(tui): approvals queue in detail pane + answer keys"
```

---

## Task 14: Docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the toggle**

In `README.md`, under `## Environment variables`, add a row/line for `AGENTCTL_APPROVALS`:

```
- `AGENTCTL_APPROVALS` — enable the approvals inbox (parse + one-click answer for
  recognized tool-permission prompts; web AttentionQueue buttons + TUI ⏳ row).
  Off by default; set `1`/`on`/`true`. Unrecognized prompts always route to attach.
```

- [ ] **Step 2: Full build, test, and web bundle**

Run:
```bash
go build ./... && go test ./... && (cd web && npm test && npm run build)
```
Expected: all green; `web/dist` rebuilt.

- [ ] **Step 3: Manual smoke (left to the user)**

```bash
# Rebuild + reinstall the daemon so it embeds the new web bundle and serves the routes:
make reinstall
# Enable the feature for the daemon (launchd: add to the plist's EnvironmentVariables,
# or for a foreground run):
AGENTCTL_APPROVALS=on agentctl daemon
```
Then: spawn an agent that hits a Bash permission prompt; confirm it appears in the web AttentionQueue with `[1] Yes / [2] … / [3] No` buttons and in the TUI under `⏳ Approvals`; answer from each surface; confirm the agent proceeds and drops off the queue. Verify an unrecognized prompt shows "attach to answer".

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document AGENTCTL_APPROVALS approvals inbox"
```

---

## Verification Checklist

- [ ] `AGENTCTL_APPROVALS` off by default; endpoints disabled, UIs fall back to today's see + jump.
- [ ] `Parse` recognizes the real captured prompt and rejects freeform/no-box/single-option/non-sequential.
- [ ] `POST /approve` re-captures + re-parses, injects the digit only on fingerprint match, 409s otherwise.
- [ ] Web AttentionQueue shows option buttons (recognized) or attach fallback (unrecognized).
- [ ] TUI `⏳ Approvals (N)` row present when enabled (even at N=0), hidden when disabled; queue renders in the detail pane; passive (count/queue update live, selection never moves on its own); 1-9 answers, `a` attaches.
- [ ] `go test ./...`, `go build ./...`, and `cd web && npm test && npm run build` all pass.
