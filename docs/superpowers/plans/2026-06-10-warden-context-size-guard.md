# Context-Size Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Passively track each live agent's context-window occupancy from its transcript and act on two configurable thresholds — display a state-colored token figure, alert at the warning threshold (default 200k), and auto-`/compact` at the critical threshold (default 400k) when the agent is idle.

**Architecture:** A new pure `internal/ctxtokens` package extracts the latest-turn context fill from the transcript JSONL and classifies it into `ok|warning|critical`. New `store.Session` fields persist the gauge. The poller, on a throttled cadence, reads each live agent's tokens via new `Deps` methods, classifies, persists, fires an alert on a threshold crossing, and sends `/compact` (via `lifecycle.Input`) at critical when the agent is idle — all gated by independent env flags that default on. The gauge rides the existing session JSON to the CLI `ls`, the TUI list, and the web grid, each rendering it green/orange/red.

**Tech Stack:** Go (stdlib `bufio`/`encoding/json`, `github.com/spf13/cobra`, `github.com/charmbracelet/lipgloss`), TypeScript/React (web), spec at `docs/superpowers/specs/2026-06-10-warden-context-size-guard-design.md`.

---

## File Structure

**Create:**
- `internal/ctxtokens/ctxtokens.go` — pure: `LatestContextTokens(io.Reader)` + `Classify` + `State` type/constants.
- `internal/ctxtokens/ctxtokens_test.go` — fixtures + table tests.
- `internal/poller/context.go` — poller context-check logic: pure `decideContext` + `checkContext` IO method + new `Poller` fields.
- `internal/poller/context_test.go` — `decideContext` table + `checkContext` integration via fake deps.
- `web/src/lib/context.ts` — `fmtTokens` + `contextClass` helpers.
- `web/src/components/ContextBadge.tsx` — colored token badge.

**Modify:**
- `internal/store/types.go` — 4 new `Session` fields + `ContextState` constants.
- `internal/store/file.go` — `UpdateContext` method.
- `internal/store/store.go` — `UpdateContext` on the `Store` interface.
- `internal/poller/poller.go` — call `checkContext` from `tick`; prune `lastCtxCheck`.
- `internal/config/config.go` — 5 new config fields + parsing.
- `internal/daemon/poller_deps.go` — implement new `Deps` methods (`ContextTokens`, `UpdateContext`, `Compact`).
- `internal/daemon/notify_hook.go` — `ContextAlertMessage` builder.
- `internal/cli/daemon.go` — wire poller token fields + `OnContextAlert`.
- `internal/cli/sessions.go` — `CONTEXT` column in `ls`.
- `internal/tui/list.go` — token cell in `renderItemLine`.
- `internal/tui/styles.go` — green/orange/red context styles.
- `web/src/lib/types.ts` — 4 new `Session` fields.
- `web/src/components/AgentGrid.tsx` — render `ContextBadge`.
- `web/src/styles/app.css` — `.ctx-*` classes.

---

## Task 1: `ctxtokens` package — extract + classify (pure)

**Files:**
- Create: `internal/ctxtokens/ctxtokens.go`
- Test: `internal/ctxtokens/ctxtokens_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package ctxtokens

import (
	"strings"
	"testing"
)

func TestLatestContextTokensSumsLatestUsage(t *testing.T) {
	// Two assistant turns; the LAST one's usage is what counts.
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":3}}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":7,"output_tokens":4}}}`
	got, ok := LatestContextTokens(strings.NewReader(jsonl))
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if want := 20 + 300 + 7; got != want {
		t.Fatalf("tokens=%d, want %d", got, want)
	}
}

func TestLatestContextTokensNoUsageReturnsFalse(t *testing.T) {
	// Freshly spawned: a user line but no assistant turn with usage yet.
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"summary","summary":"x"}`
	if _, ok := LatestContextTokens(strings.NewReader(jsonl)); ok {
		t.Fatal("ok=true, want false (no usage)")
	}
}

func TestLatestContextTokensSkipsMalformedLines(t *testing.T) {
	jsonl := `not json at all
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
{ broken`
	got, ok := LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 50 {
		t.Fatalf("got=%d ok=%v, want 50 true", got, ok)
	}
}

func TestClassify(t *testing.T) {
	const warn, crit = 200, 400
	cases := []struct {
		tokens int
		want   State
	}{
		{0, StateOK},
		{199, StateOK},
		{200, StateWarning}, // boundary: warn is inclusive
		{399, StateWarning},
		{400, StateCritical}, // boundary: crit is inclusive
		{1000, StateCritical},
	}
	for _, c := range cases {
		if got := Classify(c.tokens, warn, crit); got != c.want {
			t.Errorf("Classify(%d)=%q, want %q", c.tokens, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ctxtokens/`
Expected: FAIL — `undefined: LatestContextTokens` / `undefined: Classify`.

- [ ] **Step 3: Write the implementation**

Create `internal/ctxtokens/ctxtokens.go`:

```go
// Package ctxtokens reads an agent's current context-window occupancy from its
// Claude Code transcript JSONL and classifies it against warn/critical
// thresholds. The gauge is the most recent assistant turn's input + cached
// tokens — the same quantity /context reports, obtained passively (no keystroke
// injection, no TUI scraping).
package ctxtokens

import (
	"bufio"
	"encoding/json"
	"io"
)

// State is an agent's context-fill band.
type State string

const (
	StateOK       State = "ok"
	StateWarning  State = "warning"
	StateCritical State = "critical"
)

type usageRecord struct {
	Type    string `json:"type"`
	Message struct {
		Usage *struct {
			InputTokens         int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// LatestContextTokens scans a transcript JSONL stream and returns the context
// fill of the LAST assistant turn that carried a usage block:
// input_tokens + cache_read_input_tokens + cache_creation_input_tokens.
// ok=false means no model turn has been recorded yet (a just-spawned agent),
// in which case tokens is 0 and callers should treat the gauge as unknown.
// Malformed lines are skipped (not fatal); only the scanner's own error band is
// silently treated as end-of-data, yielding the best partial result.
func LatestContextTokens(r io.Reader) (tokens int, ok bool) {
	sc := bufio.NewScanner(r)
	// Transcript lines (tool_result payloads) can be large; match digest's cap.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec usageRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message.Usage == nil {
			continue
		}
		u := rec.Message.Usage
		tokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		ok = true // keep overwriting; the last assistant usage wins
	}
	return tokens, ok
}

// Classify maps a token count to a state. warn and crit are inclusive lower
// bounds: tokens >= crit is critical, tokens >= warn (but < crit) is warning.
func Classify(tokens, warn, crit int) State {
	switch {
	case tokens >= crit:
		return StateCritical
	case tokens >= warn:
		return StateWarning
	default:
		return StateOK
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ctxtokens/`
Expected: PASS (gofmt the struct tags if `go vet` complains about alignment — run `gofmt -w internal/ctxtokens/ctxtokens.go`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/ctxtokens/ctxtokens.go
git add internal/ctxtokens/
git commit -m "feat(ctxtokens): extract + classify context-window occupancy from transcript"
```

---

## Task 2: `store.Session` gauge fields + `UpdateContext`

**Files:**
- Modify: `internal/store/types.go` (Session struct, new constants)
- Modify: `internal/store/file.go` (UpdateContext method)
- Modify: `internal/store/store.go` (interface)
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/file_test.go`:

```go
func TestUpdateContextPersistsAndEventsOnTransition(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close(context.Background())
	ctx := context.Background()
	s := &Session{ID: "agent-ctx1", Status: StatusWorking}
	if err := fs.Insert(ctx, s); err != nil {
		t.Fatal(err)
	}

	// First write: "" -> warning is a transition, so it appends one event.
	if err := fs.UpdateContext(ctx, "agent-ctx1", 210000, "warning"); err != nil {
		t.Fatal(err)
	}
	got, _ := fs.Get(ctx, "agent-ctx1")
	if got.ContextTokens != 210000 || got.ContextState != "warning" {
		t.Fatalf("tokens=%d state=%q", got.ContextTokens, got.ContextState)
	}
	if got.ContextCheckedAt.IsZero() {
		t.Fatal("ContextCheckedAt not stamped")
	}
	if len(got.Events) != 1 {
		t.Fatalf("events=%d, want 1 on transition", len(got.Events))
	}

	// Same state, new token count: updates tokens, NO new event.
	if err := fs.UpdateContext(ctx, "agent-ctx1", 220000, "warning"); err != nil {
		t.Fatal(err)
	}
	got, _ = fs.Get(ctx, "agent-ctx1")
	if got.ContextTokens != 220000 {
		t.Fatalf("tokens=%d, want 220000", got.ContextTokens)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events=%d, want still 1 (no transition)", len(got.Events))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestUpdateContext`
Expected: FAIL — `got.ContextTokens undefined` / `fs.UpdateContext undefined`.

- [ ] **Step 3a: Add the Session fields**

In `internal/store/types.go`, inside `type Session struct`, after the `LastRestartAt` field, add:

```go
	ContextTokens    int       `json:"context_tokens,omitempty"`     // latest context-window fill; 0 = unknown (no model turn yet)
	ContextState     string    `json:"context_state,omitempty"`      // "" | ok | warning | critical
	ContextCheckedAt time.Time `json:"context_checked_at,omitempty"` // when ContextTokens was last refreshed
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`   // when warden last auto-sent /compact (cooldown guard)
```

Below the `Status` constants block in `types.go`, add context-state constants (used by callers; kept as plain strings to match the JSON field):

```go
// Context-fill states stored in Session.ContextState. They mirror
// ctxtokens.State but are duplicated here to keep store free of that import.
const (
	ContextOK       = "ok"
	ContextWarning  = "warning"
	ContextCritical = "critical"
)
```

- [ ] **Step 3b: Add the store method**

In `internal/store/file.go`, after `UpdatePane` (line ~283), add:

```go
// UpdateContext persists an agent's context-window gauge. It stamps
// ContextCheckedAt, and appends a single "context" event ONLY when the state
// band actually changes (e.g. ok→warning), so steady-state refreshes don't grow
// the event log.
func (fs *FileStore) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	return fs.mutate(id, func(s *Session) {
		if state != "" && state != s.ContextState {
			s.Events = append(s.Events, Event{
				TS:     time.Now().UTC(),
				Type:   "context",
				Detail: fmt.Sprintf("context %s→%s (%dk)", orNone(s.ContextState), state, tokens/1000),
			})
		}
		s.ContextTokens = tokens
		s.ContextState = state
		s.ContextCheckedAt = time.Now().UTC()
	})
}

// orNone renders an empty prior state as "none" for the transition event.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
```

(`fmt` is already imported in `file.go` — it is used by `exitDetail`.)

- [ ] **Step 3c: Add to the Store interface**

In `internal/store/store.go`, inside `type Store interface`, after the `UpdatePane` line, add:

```go
	// UpdateContext persists the context-window gauge (tokens + state band),
	// appending a "context" event only on a state transition.
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestUpdateContext`
Expected: PASS. Then `go build ./...` to confirm the interface addition didn't break other `Store` implementations (there is only `FileStore`).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/store/
git add internal/store/
git commit -m "feat(store): add Session context gauge fields + UpdateContext"
```

---

## Task 3: poller context-check logic (pure decision + IO)

**Files:**
- Create: `internal/poller/context.go`
- Test: `internal/poller/context_test.go`
- Modify: `internal/poller/poller.go` (Deps interface; call site; prune)

- [ ] **Step 1: Write the failing tests**

Create `internal/poller/context_test.go`:

```go
package poller

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

func TestDecideContext(t *testing.T) {
	const cool = 2 * time.Minute
	cases := []struct {
		name        string
		prev, cur   ctxtokens.State
		status      store.Status
		sinceCmpct  time.Duration
		warnAlert   bool
		autoCompact bool
		wantAlert   bool
		wantCompact bool
	}{
		{"ok->warning alerts", ctxtokens.StateOK, ctxtokens.StateWarning, store.StatusWorking, time.Hour, true, true, true, false},
		{"warning steady no alert", ctxtokens.StateWarning, ctxtokens.StateWarning, store.StatusWorking, time.Hour, true, true, false, false},
		{"warning->critical alerts, working defers compact", ctxtokens.StateWarning, ctxtokens.StateCritical, store.StatusWorking, time.Hour, true, true, true, false},
		{"critical idle compacts (deferred case, no edge)", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusIdle, time.Hour, true, true, false, true},
		{"critical waiting compacts", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusWaitingForInput, time.Hour, true, true, false, true},
		{"critical idle within cooldown skips compact", ctxtokens.StateCritical, ctxtokens.StateCritical, store.StatusIdle, 30 * time.Second, true, true, false, false},
		{"warnAlert off suppresses alert", ctxtokens.StateOK, ctxtokens.StateWarning, store.StatusWorking, time.Hour, false, true, false, false},
		{"autoCompact off suppresses compact", ctxtokens.StateWarning, ctxtokens.StateCritical, store.StatusIdle, time.Hour, true, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideContext(c.prev, c.cur, c.status, c.sinceCmpct, cool, c.warnAlert, c.autoCompact)
			if d.Alert != c.wantAlert || d.Compact != c.wantCompact {
				t.Fatalf("alert=%v compact=%v, want alert=%v compact=%v", d.Alert, d.Compact, c.wantAlert, c.wantCompact)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/ -run TestDecideContext`
Expected: FAIL — `undefined: decideContext`.

- [ ] **Step 3a: Add the Deps methods**

In `internal/poller/poller.go`, inside `type Deps interface`, after `ClearExit`, add:

```go
	// ContextTokens returns the agent's current context-window occupancy read
	// from its transcript. ok=false when no model turn has been recorded yet.
	ContextTokens(ctx context.Context, s *store.Session) (tokens int, ok bool)
	// UpdateContext persists the gauge (tokens + state band).
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// Compact sends "/compact" to the agent (only called when it is idle/waiting).
	Compact(ctx context.Context, s *store.Session) error
```

- [ ] **Step 3b: Write `internal/poller/context.go`**

```go
package poller

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

// ctxDecision is the outcome of evaluating an agent's context state this tick.
type ctxDecision struct {
	Alert   bool // fire the threshold-crossing notification
	Compact bool // send /compact now
}

// decideContext is the pure policy for the context-size guard.
//
//   - Alert fires once per upward crossing into warning or critical (cur != prev
//     and cur is a threshold band), when the warn-alert flag is on.
//   - Compact fires whenever the agent is critical AND idle/waiting AND the
//     auto-compact flag is on AND the cooldown since the last /compact has
//     elapsed. It deliberately does NOT require an edge: that lets a compact that
//     was deferred while the agent was "working" fire on a later tick once it
//     goes idle, while the cooldown prevents re-sending before a just-issued
//     compaction shows up in the transcript.
func decideContext(prev, cur ctxtokens.State, status store.Status, sinceCompact, cooldown time.Duration, warnAlert, autoCompact bool) ctxDecision {
	var d ctxDecision
	if warnAlert && cur != prev && (cur == ctxtokens.StateWarning || cur == ctxtokens.StateCritical) {
		d.Alert = true
	}
	idle := status == store.StatusIdle || status == store.StatusWaitingForInput
	if autoCompact && cur == ctxtokens.StateCritical && idle && sinceCompact >= cooldown {
		d.Compact = true
	}
	return d
}

// checkContext reads the agent's context tokens (throttled by the caller),
// classifies them, persists the gauge, and applies decideContext: it fires the
// alert hook and/or sends /compact. It is called only for live, non-terminal
// sessions. A read with ok=false (no model turn yet) is a no-op.
func (p *Poller) checkContext(ctx context.Context, s *store.Session, now time.Time) {
	tokens, ok := p.deps.ContextTokens(ctx, s)
	if !ok {
		return
	}
	cur := ctxtokens.Classify(tokens, p.TokenWarn, p.TokenCrit)
	prev := ctxtokens.State(s.ContextState)
	if err := p.deps.UpdateContext(ctx, s.ID, tokens, string(cur)); err == nil {
		s.ContextState = string(cur) // keep the snapshot coherent for this tick
		s.ContextTokens = tokens
	}

	sinceCompact := p.CompactCooldown // default to "elapsed" when never compacted
	if s.LastCompactAt != nil {
		sinceCompact = now.Sub(*s.LastCompactAt)
	}
	d := decideContext(prev, cur, s.Status, sinceCompact, p.CompactCooldown, p.WarnAlert, p.AutoCompact)

	if d.Alert && p.OnContextAlert != nil {
		p.OnContextAlert(s, cur, tokens)
	}
	if d.Compact {
		if err := p.deps.Compact(ctx, s); err == nil {
			t := now
			s.LastCompactAt = &t
			_ = p.deps.UpdateContext(ctx, s.ID, tokens, string(cur)) // re-persist isn't needed for LastCompactAt
		}
	}
}
```

> **Note for the implementer:** `LastCompactAt` must be persisted so the cooldown survives across ticks/restarts. The cleanest way is a tiny dedicated store write. Add it in Step 3c rather than overloading `UpdateContext`.

- [ ] **Step 3c: Persist `LastCompactAt`**

Add a store method so the cooldown survives. In `internal/store/file.go` after `UpdateContext`:

```go
// StampCompact records that warden auto-sent /compact to id just now (cooldown
// guard for the context-size guard).
func (fs *FileStore) StampCompact(ctx context.Context, id string) error {
	return fs.mutate(id, func(s *Session) {
		now := time.Now().UTC()
		s.LastCompactAt = &now
	})
}
```

Add to the `Store` interface in `internal/store/store.go` after `UpdateContext`:

```go
	// StampCompact records the time of an auto-/compact (cooldown guard).
	StampCompact(ctx context.Context, id string) error
```

Add to the poller `Deps` interface in `poller.go` after `Compact`:

```go
	// StampCompact records that /compact was just sent (cooldown guard).
	StampCompact(ctx context.Context, id string) error
```

Now fix the compact branch in `context.go` to use it:

```go
	if d.Compact {
		if err := p.deps.Compact(ctx, s); err == nil {
			t := now
			s.LastCompactAt = &t
			_ = p.deps.StampCompact(ctx, s.ID)
		}
	}
```

- [ ] **Step 3d: Add the new `Poller` fields**

In `internal/poller/poller.go`, add to the `Poller` struct (after `OnTransition`):

```go
	// Context-size guard config + hooks (set by the daemon after New). When
	// TokenGuard is false the whole check is skipped. CompactCooldown bounds how
	// often /compact may be auto-sent to one agent.
	TokenGuard      bool
	TokenWarn       int
	TokenCrit       int
	WarnAlert       bool
	AutoCompact     bool
	CompactCooldown time.Duration
	CheckEvery      time.Duration // throttle for the per-agent transcript read
	// OnContextAlert, if set, fires once per upward threshold crossing.
	OnContextAlert func(sess *store.Session, state ctxtokens.State, tokens int)

	lastCtxCheck map[string]time.Time // last context read per session (tick goroutine only)
```

Add the import `"github.com/srjn45/warden/internal/ctxtokens"` to `poller.go`. In `New`, initialise the map and the throttle default:

```go
func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:           d,
		stuckAfter:     stuckAfter,
		SummarizeAfter: 2 * time.Minute,
		lastSummary:    map[string]time.Time{},
		inflight:       map[string]struct{}{},
		lastCtxCheck:   map[string]time.Time{},
		CheckEvery:     20 * time.Second,
		CompactCooldown: 2 * time.Minute,
	}
}
```

- [ ] **Step 3e: Call `checkContext` from `tick`**

In `internal/poller/poller.go` `tick`, inside the per-session loop, immediately after the `if alive && paneChanged && ...` summary-dispatch block (line ~182, before the loop's closing brace), add:

```go
		if p.TokenGuard && alive && p.CheckEvery >= 0 && now.Sub(p.lastCtxCheck[s.ID]) >= p.CheckEvery {
			p.lastCtxCheck[s.ID] = now
			p.checkContext(ctx, s, now)
		}
```

In `pruneSummaryState`, also prune `lastCtxCheck` (rename intent: it prunes all per-session maps). Replace its body's delete loop to also clear `lastCtxCheck`:

```go
	for id := range p.lastSummary {
		if _, ok := live[id]; !ok {
			delete(p.lastSummary, id)
		}
	}
	for id := range p.lastCtxCheck {
		if _, ok := live[id]; !ok {
			delete(p.lastCtxCheck, id)
		}
	}
```

(Guard the early-return at the top of `pruneSummaryState` so it doesn't skip when only `lastCtxCheck` is non-empty: change `if len(p.lastSummary) == 0 {` to `if len(p.lastSummary) == 0 && len(p.lastCtxCheck) == 0 {`.)

- [ ] **Step 4: Run the pure-decision test**

Run: `go test ./internal/poller/ -run TestDecideContext`
Expected: PASS. Then `go build ./...` — this WILL fail until Task 5 updates the daemon's `pollerDeps` to implement the three new `Deps` methods, and any poller-test fake deps. That's expected; proceed to Step 5 to commit the pure logic, then fix builds in Task 5.

> If existing `internal/poller/*_test.go` fakes implement `Deps` and now fail to compile, add no-op implementations to them in this task: `ContextTokens` returns `(0, false)`, `UpdateContext`/`Compact`/`StampCompact` return `nil`. Grep: `grep -rln "func.*ContextTokens\|fakeDeps\|stubDeps\|type .*Deps" internal/poller/*_test.go` and add the methods to each fake.

- [ ] **Step 5: Add a `checkContext` integration test via fake deps**

Append to `internal/poller/context_test.go` a fake implementing `Deps` (or reuse the package's existing fake if present — grep first). Minimal standalone fake:

```go
type ctxFakeDeps struct {
	tokens      int
	tokensOK    bool
	updated     []string // "tokens:state"
	compacted   int
	stamped     int
}

func (f *ctxFakeDeps) List(context.Context) ([]*store.Session, error) { return nil, nil }
func (f *ctxFakeDeps) UpdateStatusIf(context.Context, string, store.Status, store.Status) (bool, error) { return false, nil }
func (f *ctxFakeDeps) UpdatePane(context.Context, string, string) error   { return nil }
func (f *ctxFakeDeps) UpdateSubject(context.Context, string, string) error { return nil }
func (f *ctxFakeDeps) SessionAlive(context.Context, string) bool          { return true }
func (f *ctxFakeDeps) CapturePane(context.Context, string) (string, error) { return "", nil }
func (f *ctxFakeDeps) Summarize(context.Context, *store.Session) (string, error) { return "", nil }
func (f *ctxFakeDeps) ExitCode(context.Context, string) (int, bool)       { return 0, false }
func (f *ctxFakeDeps) FinalizeExit(context.Context, string, store.Status, store.Status, int) (bool, error) { return false, nil }
func (f *ctxFakeDeps) ClearExit(context.Context, string)                  {}
func (f *ctxFakeDeps) ContextTokens(context.Context, *store.Session) (int, bool) { return f.tokens, f.tokensOK }
func (f *ctxFakeDeps) UpdateContext(_ context.Context, _ string, tokens int, state string) error {
	f.updated = append(f.updated, fmt.Sprintf("%d:%s", tokens, state))
	return nil
}
func (f *ctxFakeDeps) Compact(context.Context, *store.Session) error { f.compacted++; return nil }
func (f *ctxFakeDeps) StampCompact(context.Context, string) error    { f.stamped++; return nil }

func TestCheckContextCriticalIdleCompactsAndPersists(t *testing.T) {
	fd := &ctxFakeDeps{tokens: 420000, tokensOK: true}
	p := New(fd, time.Minute)
	p.TokenWarn, p.TokenCrit = 200000, 400000
	p.WarnAlert, p.AutoCompact, p.TokenGuard = true, true, true
	var alerts int
	p.OnContextAlert = func(*store.Session, ctxtokens.State, int) { alerts++ }

	s := &store.Session{ID: "a1", Status: store.StatusIdle, ContextState: ""}
	p.checkContext(context.Background(), s, time.Now())

	if len(fd.updated) == 0 {
		t.Fatal("gauge not persisted")
	}
	if alerts != 1 {
		t.Fatalf("alerts=%d, want 1 (\"\"→critical is a crossing)", alerts)
	}
	if fd.compacted != 1 || fd.stamped != 1 {
		t.Fatalf("compacted=%d stamped=%d, want 1/1", fd.compacted, fd.stamped)
	}
}

func TestCheckContextNoUsageIsNoop(t *testing.T) {
	fd := &ctxFakeDeps{tokensOK: false}
	p := New(fd, time.Minute)
	p.TokenGuard, p.AutoCompact, p.WarnAlert = true, true, true
	p.checkContext(context.Background(), &store.Session{ID: "a1", Status: store.StatusIdle}, time.Now())
	if len(fd.updated) != 0 || fd.compacted != 0 {
		t.Fatal("no-usage read must be a no-op")
	}
}
```

Add `"fmt"` to the test imports.

- [ ] **Step 6: Run poller tests**

Run: `go test ./internal/poller/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/poller/ internal/store/
git add internal/poller/ internal/store/
git commit -m "feat(poller): context-size guard — read, classify, alert, auto-compact when idle"
```

---

## Task 4: config flags (5, all default on)

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestTokenGuardDefaults(t *testing.T) {
	for _, k := range []string{"WARDEN_TOKEN_GUARD", "WARDEN_TOKEN_WARN_ALERT", "WARDEN_TOKEN_AUTO_COMPACT", "WARDEN_TOKEN_WARN", "WARDEN_TOKEN_CRITICAL", "AGENTCTL_TOKEN_GUARD"} {
		t.Setenv(k, "")
	}
	c := Load()
	if !c.TokenGuard || !c.TokenWarnAlert || !c.TokenAutoCompact {
		t.Fatalf("guard=%v warnAlert=%v autoCompact=%v, want all true", c.TokenGuard, c.TokenWarnAlert, c.TokenAutoCompact)
	}
	if c.TokenWarn != 200000 || c.TokenCritical != 400000 {
		t.Fatalf("warn=%d crit=%d, want 200000/400000", c.TokenWarn, c.TokenCritical)
	}
}

func TestTokenGuardOverrides(t *testing.T) {
	t.Setenv("WARDEN_TOKEN_AUTO_COMPACT", "off")
	t.Setenv("WARDEN_TOKEN_WARN", "100000")
	t.Setenv("WARDEN_TOKEN_CRITICAL", "150000")
	c := Load()
	if c.TokenAutoCompact {
		t.Fatal("auto-compact should be off")
	}
	if c.TokenWarn != 100000 || c.TokenCritical != 150000 {
		t.Fatalf("warn=%d crit=%d", c.TokenWarn, c.TokenCritical)
	}
}

func TestTokenThresholdsFallBackWhenInverted(t *testing.T) {
	t.Setenv("WARDEN_TOKEN_WARN", "500000")
	t.Setenv("WARDEN_TOKEN_CRITICAL", "400000") // crit <= warn → defaults
	c := Load()
	if c.TokenWarn != 200000 || c.TokenCritical != 400000 {
		t.Fatalf("inverted config not reset: warn=%d crit=%d", c.TokenWarn, c.TokenCritical)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestToken`
Expected: FAIL — `c.TokenGuard undefined`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to the `Config` struct:

```go
	TokenGuard       bool
	TokenWarnAlert   bool
	TokenAutoCompact bool
	TokenWarn        int
	TokenCritical    int
```

Add the boolean helpers (default-on pattern, matching `metricsEnabled`):

```go
// onByDefault reads a WARDEN_<name> (legacy AGENTCTL_<name>) boolean that
// defaults ON, disabled only by 0/off/false.
func onByDefault(name string) bool {
	switch strings.ToLower(env(name)) {
	case "0", "off", "false":
		return false
	}
	return true
}

// envInt reads a WARDEN_<name> integer, returning def when unset/unparseable.
func envInt(name string, def int) int {
	if v := env(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```

In `Load()`, compute the thresholds with the inverted-config guard and add the fields:

```go
	tWarn := envInt("TOKEN_WARN", 200000)
	tCrit := envInt("TOKEN_CRITICAL", 400000)
	if tCrit <= tWarn { // inverted/degenerate config → defaults (warning must be reachable)
		tWarn, tCrit = 200000, 400000
	}
	return Config{
		// ... existing fields ...
		TokenGuard:       onByDefault("TOKEN_GUARD"),
		TokenWarnAlert:   onByDefault("TOKEN_WARN_ALERT"),
		TokenAutoCompact: onByDefault("TOKEN_AUTO_COMPACT"),
		TokenWarn:        tWarn,
		TokenCritical:    tCrit,
	}
```

(`strconv` is already imported in `config.go`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestToken`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/
git add internal/config/
git commit -m "feat(config): token-guard flags (guard/warn-alert/auto-compact + thresholds)"
```

---

## Task 5: daemon wiring (Deps impl + poller config + alert message)

**Files:**
- Modify: `internal/daemon/poller_deps.go`
- Modify: `internal/daemon/notify_hook.go`
- Test: `internal/daemon/notify_hook_test.go`
- Modify: `internal/cli/daemon.go`

- [ ] **Step 1: Write the failing test (alert message builder)**

Append to `internal/daemon/notify_hook_test.go`:

```go
func TestContextAlertMessage(t *testing.T) {
	s := &store.Session{ID: "agent-x", Subject: "refactor auth"}
	title, body := ContextAlertMessage(s, ctxtokens.StateWarning, 210000)
	if title == "" || body == "" {
		t.Fatal("empty message")
	}
	if !strings.Contains(body, "agent-x") || !strings.Contains(body, "210k") {
		t.Fatalf("body missing id/size: %q", body)
	}
	tCrit, bCrit := ContextAlertMessage(s, ctxtokens.StateCritical, 410000)
	if !strings.Contains(strings.ToLower(tCrit+bCrit), "critical") {
		t.Fatalf("critical message should say critical: %q / %q", tCrit, bCrit)
	}
}
```

Add imports `"strings"`, `"github.com/srjn45/warden/internal/ctxtokens"` to the test file if absent.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestContextAlertMessage`
Expected: FAIL — `undefined: ContextAlertMessage`.

- [ ] **Step 3a: Add `ContextAlertMessage`**

In `internal/daemon/notify_hook.go`, add (and add imports `"fmt"`, `"github.com/srjn45/warden/internal/ctxtokens"`):

```go
// ContextAlertMessage builds the notification for an agent crossing a
// context-size threshold. Warning nudges the user to compact; critical notes
// that warden will auto-/compact once the agent is idle.
func ContextAlertMessage(sess *store.Session, state ctxtokens.State, tokens int) (title, body string) {
	subj := sess.Subject
	if subj == "" {
		subj = sess.ID
	}
	size := fmt.Sprintf("%dk", tokens/1000)
	switch state {
	case ctxtokens.StateCritical:
		return "warden — context critical",
			fmt.Sprintf("%s at %s (%s) — auto-/compact when idle", sess.ID, size, subj)
	default: // warning
		return "warden — context high",
			fmt.Sprintf("%s at %s (%s) — consider /compact", sess.ID, size, subj)
	}
}
```

- [ ] **Step 3b: Implement the new `Deps` methods on `pollerDeps`**

In `internal/daemon/poller_deps.go`, add (and add imports `"os"`, `"github.com/srjn45/warden/internal/ctxtokens"`):

```go
// ContextTokens reads the agent's current context-window occupancy from its
// transcript JSONL. ok=false when the transcript is missing or has no model
// turn yet (a just-spawned agent).
func (d *pollerDeps) ContextTokens(_ context.Context, s *store.Session) (int, bool) {
	path := d.lc.TranscriptPath(s)
	if path == "" {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return ctxtokens.LatestContextTokens(f)
}

func (d *pollerDeps) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	return d.store.UpdateContext(ctx, id, tokens, state)
}

// Compact sends "/compact" to the agent's tmux pane via the bracketed-paste +
// Enter path. Called only when the agent is idle/waiting.
func (d *pollerDeps) Compact(ctx context.Context, s *store.Session) error {
	return d.lc.Input(ctx, s.ID, "/compact")
}

func (d *pollerDeps) StampCompact(ctx context.Context, id string) error {
	return d.store.StampCompact(ctx, id)
}
```

- [ ] **Step 3c: Wire poller config + alert hook in `cli/daemon.go`**

In `internal/cli/daemon.go`, after `pl := poller.New(pd, 5*time.Minute)` (line 68), add:

```go
				pl.TokenGuard = cfg.TokenGuard
				pl.TokenWarn = cfg.TokenWarn
				pl.TokenCrit = cfg.TokenCritical
				pl.WarnAlert = cfg.TokenWarnAlert
				pl.AutoCompact = cfg.TokenAutoCompact
```

After the `notifyHook := daemon.NotifyOnTransition(...)` line (line 90), wire the context alert to the same notifier:

```go
				ctxNotifier := notify.New(cfg.NotifyEnabled)
				pl.OnContextAlert = func(sess *store.Session, state ctxtokens.State, tokens int) {
					title, body := daemon.ContextAlertMessage(sess, state, tokens)
					go ctxNotifier.Notify(title, body)
				}
```

Add the import `"github.com/srjn45/warden/internal/ctxtokens"` to `cli/daemon.go`.

- [ ] **Step 4: Build + test the whole tree**

Run: `go build ./... && go test ./internal/daemon/ ./internal/poller/ ./internal/cli/`
Expected: PASS. (If `go build` flags a poller test fake missing the new `Deps` methods, you missed the note in Task 3 Step 4 — add the no-op methods.)

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/daemon/ internal/cli/
git add internal/daemon/ internal/cli/
git commit -m "feat(daemon): wire context-size guard (transcript read, /compact send, alert)"
```

---

## Task 6: CLI `ls` — colored CONTEXT column

**Files:**
- Modify: `internal/cli/sessions.go`
- Test: `internal/cli/sessions_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create/append `internal/cli/sessions_test.go`:

```go
package cli

import "testing"

func TestContextCell(t *testing.T) {
	cases := []struct {
		tokens int
		state  string
		want   string
	}{
		{0, "", "—"},
		{145000, "ok", "145k"},
		{210000, "warning", "210k"},
		{410000, "critical", "410k"},
	}
	for _, c := range cases {
		if got := contextCell(c.tokens, c.state, false); got != c.want {
			t.Errorf("contextCell(%d,%q)=%q, want %q", c.tokens, c.state, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestContextCell`
Expected: FAIL — `undefined: contextCell`.

- [ ] **Step 3: Implement**

In `internal/cli/sessions.go`, add (the `color` param colorizes only for a TTY; pass `false` from tests for stable plain output). Add the import `"github.com/fatih/color"` only if the repo already uses it — otherwise use raw ANSI. Grep first: `grep -rn "fatih/color\|\\\\033\[" internal/cli/`. If neither, use raw ANSI escapes as below:

```go
// contextCell formats an agent's context-window gauge for the ls table. An
// unknown gauge (no model turn yet) renders "—". When color is true (stdout is
// a TTY) the figure is tinted green/orange/red by state.
func contextCell(tokens int, state string, color bool) string {
	if tokens == 0 && state == "" {
		return "—"
	}
	s := fmt.Sprintf("%dk", tokens/1000)
	if !color {
		return s
	}
	switch state {
	case store.ContextWarning:
		return "\033[33m" + s + "\033[0m" // orange/yellow
	case store.ContextCritical:
		return "\033[31m" + s + "\033[0m" // red
	default:
		return "\033[32m" + s + "\033[0m" // green
	}
}
```

In `newLsCmd`, add the column. Change the header and row lines:

```go
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
				color := isTTY(cmd.OutOrStdout())
				fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tCONTEXT\tAGE\tDIR\tSUBJECT")
				for _, s := range sessions {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						s.ID, typeOrPending(s.Type), s.Status, contextCell(s.ContextTokens, s.ContextState, color),
						age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
				}
```

Add a TTY helper if one doesn't exist (grep `grep -rn "func isTTY\|term.IsTerminal\|x/term" internal/cli/`); if absent, add:

```go
// isTTY reports whether w is a terminal (for opt-in ANSI coloring).
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
```

Add imports `"io"`, `"os"` to `sessions.go` if not present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestContextCell && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/cli/
git add internal/cli/
git commit -m "feat(cli): show colored CONTEXT column in ls"
```

---

## Task 7: TUI — colored token cell in the agent row

**Files:**
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go` (append; create if absent)

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/list_test.go` (or create with `package tui`):

```go
func TestContextLabel(t *testing.T) {
	cases := []struct {
		tokens int
		state  string
		want   string
	}{
		{0, "", ""},
		{145000, "ok", "145k"},
		{210000, "warning", "210k"},
		{410000, "critical", "410k"},
	}
	for _, c := range cases {
		if got, _ := contextLabel(c.tokens, c.state); got != c.want {
			t.Errorf("contextLabel(%d,%q)=%q, want %q", c.tokens, c.state, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestContextLabel`
Expected: FAIL — `undefined: contextLabel`.

- [ ] **Step 3a: Add styles**

In `internal/tui/styles.go`, add to the `var (...)` block:

```go
	stCtxOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))  // green
	stCtxWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // orange/amber
	stCtxCrit = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))  // red
```

- [ ] **Step 3b: Add `contextLabel` + render it**

In `internal/tui/list.go`, add:

```go
// contextLabel renders an agent's context gauge as a short figure ("210k") plus
// the lipgloss style for its state band. An unknown gauge (no model turn yet)
// renders "" so a just-spawned agent shows nothing rather than a green 0k.
func contextLabel(tokens int, state string) (string, lipgloss.Style) {
	if tokens == 0 && state == "" {
		return "", stMuted
	}
	label := fmt.Sprintf("%dk", tokens/1000)
	switch state {
	case store.ContextWarning:
		return label, stCtxWarn
	case store.ContextCritical:
		return label, stCtxCrit
	default:
		return label, stCtxOK
	}
}
```

In `renderItemLine`, in the `default:` case (the agent row, line ~488), insert the context cell between status and age:

```go
		default:
			s := it.session
			label, st := badge(s.Status, s.ExitCode)
			cl, cst := contextLabel(s.ContextTokens, s.ContextState)
			line = fmt.Sprintf("%-12s %-9s %-11s %-6s %-5s %s",
				trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label),
				cst.Render(fmt.Sprintf("%-6s", cl)), age(s.UpdatedAt),
				trunc(s.Subject, max(0, width-51)))
```

(The subject truncation width drops from `width-44` to `width-51` to account for the new 6-wide column + space.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestContextLabel && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/tui/
git add internal/tui/
git commit -m "feat(tui): colored context-size cell on the agent row"
```

---

## Task 8: web — colored context badge

**Files:**
- Modify: `web/src/lib/types.ts`
- Create: `web/src/lib/context.ts`
- Create: `web/src/components/ContextBadge.tsx`
- Modify: `web/src/components/AgentGrid.tsx`
- Modify: `web/src/styles/app.css`

- [ ] **Step 1: Add the Session fields**

In `web/src/lib/types.ts`, inside `interface Session`, after `exit_code?: number | null;`, add:

```ts
  context_tokens?: number;
  context_state?: '' | 'ok' | 'warning' | 'critical';
  context_checked_at?: string;
  last_compact_at?: string | null;
```

- [ ] **Step 2: Add the helper lib**

Create `web/src/lib/context.ts`:

```ts
// Context-size gauge helpers for the web UI. Mirrors the daemon's gauge: a
// short "210k" figure plus a CSS class tied to the ok/warning/critical band.

export function fmtTokens(tokens?: number): string {
  if (!tokens || tokens <= 0) return '—';
  return `${Math.round(tokens / 1000)}k`;
}

export function contextClass(state?: string): string {
  switch (state) {
    case 'warning': return 'ctx-warning';
    case 'critical': return 'ctx-critical';
    case 'ok': return 'ctx-ok';
    default: return 'ctx-unknown';
  }
}

// known reports whether the agent has a usable gauge yet (a model turn ran).
export function known(tokens?: number, state?: string): boolean {
  return !!state && state !== '' && (tokens ?? 0) > 0;
}
```

- [ ] **Step 3: Add the badge component**

Create `web/src/components/ContextBadge.tsx`:

```tsx
import { fmtTokens, contextClass, known } from '../lib/context';

// ContextBadge shows an agent's context-window fill, tinted green/orange/red.
// Renders nothing when the gauge is unknown (just-spawned, no model turn yet).
export default function ContextBadge({ tokens, state }: { tokens?: number; state?: string }) {
  if (!known(tokens, state)) return null;
  return (
    <span className={`ctx-badge ${contextClass(state)}`} title={`context ~${fmtTokens(tokens)} (${state})`}>
      {fmtTokens(tokens)}
    </span>
  );
}
```

- [ ] **Step 4: Render it in the grid tile**

In `web/src/components/AgentGrid.tsx`, add the import and place the badge in `tile-head`:

```tsx
import ContextBadge from './ContextBadge';
```

Change the `tile-head` block:

```tsx
                <div className="tile-head">
                  <b>{s.id}</b> <BusyIdleBadge status={s.status} exitCode={s.exit_code} />
                  <ContextBadge tokens={s.context_tokens} state={s.context_state} />
                </div>
```

- [ ] **Step 5: Add the CSS**

In `web/src/styles/app.css`, after the `.badge.error` line (line ~30), add:

```css
.ctx-badge { font-size: .7rem; padding: .1rem .4rem; border-radius: 1rem; margin-left: .3rem; color: #fff; }
.ctx-badge.ctx-ok { background: var(--busy); }
.ctx-badge.ctx-warning { background: var(--attention); }
.ctx-badge.ctx-critical { background: var(--error); }
```

- [ ] **Step 6: Build the web bundle**

Run: `cd web && npm run build`
Expected: Type-checks and builds with no errors. (If the project has a typecheck script, also run `npm run typecheck` or `npx tsc --noEmit`.)

- [ ] **Step 7: Commit**

```bash
git add web/src/
git commit -m "feat(web): colored context-size badge on agent tiles"
```

---

## Task 9: docs + final verification

**Files:**
- Modify: `README.md` and/or `FEATURES.md` and `USAGE.md` (grep for the env-var table)

- [ ] **Step 1: Document the new env flags**

Run `grep -rln "WARDEN_METRICS\|WARDEN_NOTIFY\|WARDEN_APPROVALS" README.md USAGE.md FEATURES.md docs/` to find the config table(s). Add rows for `WARDEN_TOKEN_GUARD`, `WARDEN_TOKEN_WARN_ALERT`, `WARDEN_TOKEN_AUTO_COMPACT`, `WARDEN_TOKEN_WARN`, `WARDEN_TOKEN_CRITICAL` with the defaults (on / on / on / 200000 / 400000) and a one-line description of the context-size guard, mirroring the style of the existing rows.

- [ ] **Step 2: Full suite + build**

Run: `go build ./... && go test ./... && (cd web && npm run build)`
Expected: All green. If heavy tmux/daemon packages time out under machine contention, re-run the touched packages in isolation: `go test ./internal/ctxtokens/ ./internal/store/ ./internal/poller/ ./internal/config/ ./internal/daemon/ ./internal/cli/ ./internal/tui/`.

- [ ] **Step 3: Commit**

```bash
git add README.md USAGE.md FEATURES.md docs/ 2>/dev/null
git commit -m "docs: document context-size guard env flags"
```

- [ ] **Step 4: Manual smoke (left for user)**

After `make install` + daemon restart (the binary embeds `web/dist`, so rebuild the web bundle first):
1. Spawn a supervised agent and let it run until its transcript shows a model turn → `warden ls` shows a green `CONTEXT` figure; web tile shows a green badge; TUI row shows the green cell.
2. Set `WARDEN_TOKEN_WARN=1000 WARDEN_TOKEN_CRITICAL=2000` (tiny thresholds) on the daemon and restart → an active agent should flip orange (warning notification fires if `WARDEN_NOTIFY=1`) then red, and at red, when the agent is idle, warden sends `/compact` (verify by attaching: the composer shows the compaction running).
3. Set `WARDEN_TOKEN_AUTO_COMPACT=off` and confirm red badge + alert but NO auto-/compact. Set `WARDEN_TOKEN_GUARD=off` and confirm the column/badge goes blank and nothing fires.

---

## Self-Review notes (addressed)

- **Spec coverage:** gauge source (Task 1), Session fields (Task 2), poller classify/alert/idle-gated-compact + cooldown + throttle (Task 3), 5 independent flags incl. inverted-config fallback (Task 4), daemon wiring + `/compact` send + alert (Task 5), colored display in CLI/TUI/web (Tasks 6–8), docs (Task 9). The accepted-risk (auto-compact on by default) is realized by `TokenAutoCompact` defaulting on and gated only by idle + cooldown.
- **Type consistency:** `ctxtokens.State` (`StateOK/StateWarning/StateCritical`) is the in-poller type; `store.Context{OK,Warning,Critical}` string constants are the persisted/rendered values; web uses the literal `'ok'|'warning'|'critical'`. `decideContext`, `checkContext`, `ContextTokens`, `UpdateContext`, `Compact`, `StampCompact`, `OnContextAlert`, `ContextAlertMessage`, `contextCell`, `contextLabel`, `fmtTokens`, `contextClass` are each defined once and referenced consistently.
- **Throttle:** `CheckEvery` (default 20s) gates the per-agent transcript read so large transcripts aren't scanned every tick; `lastCtxCheck` is pruned alongside `lastSummary`.
