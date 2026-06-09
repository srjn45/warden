# Memory-Pressure-Aware Soft Spawn Gate — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Warn (never hard-block) before spawning an agent when macOS memory pressure is elevated or too many agents are already live, turning the known freeze failure mode into an informed confirm-anyway choice. Toggleable via env.

**Architecture:** A pure `internal/pressure` package owns the level model + verdict truth table. `lifecycle.MemoryPressure` reads the macOS `sysctl` level (degrading to Normal off-macOS). The daemon samples pressure on a background ticker (sibling to the poller), caches it, exposes `GET /pressure`, and gates `handleSpawn` — returning **HTTP 428** with a structured verdict when elevated and `force` is unset. Every spawn surface (CLI, MCP, web, TUI) consumes that one verdict; the in-process pipeline executor bypasses the HTTP gate by construction (advisory log only).

**Tech Stack:** Go (chi router, file store), React/TypeScript (web), Bubble Tea (TUI). Spec: `docs/superpowers/specs/2026-06-05-agentctl-spawn-gate-design.md`.

**Dependency order:** Tasks 1–6 are sequential (pure pkg → lifecycle → daemon contract → Go client). Tasks 7–11 are the surface fan-out and become **independent of each other** once Task 6 lands (CLI/MCP/TUI depend on the Go client; web depends only on the HTTP contract from Tasks 4–5). Task 12 is the final integration build.

---

## File Structure

**New files:**
- `internal/pressure/pressure.go` — pure: `Level`, `ParseSysctl`, `Verdict`, `Evaluate`.
- `internal/pressure/pressure_test.go` — unit tests for the above.
- `web/src/lib/pressure.ts` — pure formatting helper for the gauge/warning.
- `web/src/lib/pressure.test.ts` — unit test (if a web test runner exists; see Task 10).

**Modified files:**
- `internal/config/config.go` — `SpawnGateEnabled` (default ON) + `SpawnGateMaxAgents` (default 5).
- `internal/lifecycle/lifecycle.go` — `MemoryPressure` exec helper.
- `internal/daemon/api.go` — `Server` pressure-cache fields + `SetSpawnGate`; `Lifecycle` interface gains `MemoryPressure`; `SpawnRequest.Force`; response structs.
- `internal/daemon/lifecycle_adapter.go` — forward `MemoryPressure`.
- `internal/daemon/server.go` — start the sampler loop in `ListenAndServe`.
- `internal/daemon/lifecycle_routes.go` — gate in `handleSpawn`; register `GET /pressure`.
- `internal/daemon/pressure_routes.go` *(new)* — `handlePressure` + sampler method.
- `internal/cli/daemon.go` — wire `SetSpawnGate` from config.
- `internal/client/client.go` — capture error body bytes; `ErrConfirmationRequired`; `SpawnParams.Force`; `Client.Pressure`.
- `internal/cli/lifecycle.go` — `start --force`.
- `internal/mcp/server.go` — `spawn_agent` `force` arg + confirmation text.
- `internal/daemon/executor.go` — advisory pressure log before `SpawnJob`.
- `web/src/lib/api.ts` — `force` param + `getPressure` + 428 handling.
- `web/src/components/NewAgentModal.tsx` — confirm-anyway flow.
- `web/src/components/<fleet header>` — pressure gauge (locate in Task 10).
- `internal/tui/*` — new-agent confirm + header gauge (locate in Task 11).

---

## Task 1: `internal/pressure` pure package

**Files:**
- Create: `internal/pressure/pressure.go`
- Test: `internal/pressure/pressure_test.go`

- [ ] **Step 1: Write the failing test**

```go
package pressure

import "testing"

func TestParseSysctl(t *testing.T) {
	cases := []struct {
		raw  string
		want Level
		ok   bool
	}{
		{"1", Normal, true},
		{"2\n", Warn, true},
		{"4", Critical, true},
		{"kern.memorystatus_vm_pressure_level: 2", Warn, true},
		{"kern.memorystatus_vm_pressure_level: 4\n", Critical, true},
		{"", 0, false},
		{"bogus", 0, false},
		{"3", 0, false}, // macOS never emits 3; reject unmapped values
	}
	for _, c := range cases {
		got, err := ParseSysctl(c.raw)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseSysctl(%q) = (%v, %v), want (%v, nil)", c.raw, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseSysctl(%q) = (%v, nil), want error", c.raw, got)
		}
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name       string
		level      Level
		count, max int
		want       bool
	}{
		{"normal under limit", Normal, 3, 5, false},
		{"warn under limit", Warn, 3, 5, true},
		{"critical under limit", Critical, 0, 5, true},
		{"normal at limit", Normal, 5, 5, true},
		{"normal over limit", Normal, 6, 5, true},
		{"normal one under limit", Normal, 4, 5, false},
		{"count trigger disabled (max<=0)", Normal, 99, 0, false},
	}
	for _, c := range cases {
		got := Evaluate(c.level, c.count, c.max)
		if got.Elevated != c.want {
			t.Errorf("%s: Evaluate(%v,%d,%d).Elevated = %v, want %v",
				c.name, c.level, c.count, c.max, got.Elevated, c.want)
		}
		if got.Elevated && got.Reason == "" {
			t.Errorf("%s: elevated verdict must have a Reason", c.name)
		}
		if !got.Elevated && got.Reason != "" {
			t.Errorf("%s: non-elevated verdict must have empty Reason, got %q", c.name, got.Reason)
		}
	}
}

func TestLevelString(t *testing.T) {
	if Normal.String() != "normal" || Warn.String() != "warn" || Critical.String() != "critical" {
		t.Fatal("level names wrong")
	}
	if Level(99).String() != "unknown" {
		t.Fatal("unmapped level should be 'unknown'")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pressure/`
Expected: FAIL — `undefined: Level` / `undefined: ParseSysctl` / `undefined: Evaluate`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package pressure models macOS memory-pressure levels and the soft-spawn-gate
// verdict. It is pure: no exec, no I/O — parsing and the decision live here so
// they are unit-testable (mirrors internal/approval and internal/digest).
package pressure

import (
	"fmt"
	"strconv"
	"strings"
)

// Level mirrors macOS kern.memorystatus_vm_pressure_level (1=normal, 2=warn,
// 4=critical). The integer values cross the wire as-is.
type Level int

const (
	Normal   Level = 1
	Warn     Level = 2
	Critical Level = 4
)

func (l Level) String() string {
	switch l {
	case Normal:
		return "normal"
	case Warn:
		return "warn"
	case Critical:
		return "critical"
	default:
		return "unknown"
	}
}

// ParseSysctl parses a `sysctl kern.memorystatus_vm_pressure_level` reading,
// accepting either the bare value ("2") or the full line
// ("kern.memorystatus_vm_pressure_level: 2"). Returns an error for empty or
// unmapped input so the caller can degrade to Normal.
func ParseSysctl(raw string) (Level, error) {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("pressure: unparseable sysctl value %q", raw)
	}
	switch Level(n) {
	case Normal, Warn, Critical:
		return Level(n), nil
	default:
		return 0, fmt.Errorf("pressure: unmapped level %d", n)
	}
}

// Verdict is the gate decision. It crosses the wire (daemon → clients).
type Verdict struct {
	Elevated   bool   `json:"elevated"`
	Level      Level  `json:"level"`
	AgentCount int    `json:"agent_count"`
	MaxAgents  int    `json:"max_agents"`
	Reason     string `json:"reason"`
}

// Evaluate decides whether a spawn should warn. Elevated when the OS level is
// at least Warn OR the live agent count has reached maxAgents. A maxAgents <= 0
// disables the count co-trigger (level-only gating).
func Evaluate(level Level, agentCount, maxAgents int) Verdict {
	byPressure := level >= Warn
	byCount := maxAgents > 0 && agentCount >= maxAgents
	v := Verdict{
		Elevated:   byPressure || byCount,
		Level:      level,
		AgentCount: agentCount,
		MaxAgents:  maxAgents,
	}
	switch {
	case byPressure && byCount:
		v.Reason = fmt.Sprintf("pressure: %s · %d agents live ≥ %d", level, agentCount, maxAgents)
	case byPressure:
		v.Reason = fmt.Sprintf("pressure: %s", level)
	case byCount:
		v.Reason = fmt.Sprintf("%d agents live ≥ %d", agentCount, maxAgents)
	}
	return v
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pressure/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pressure/
git commit -m "feat(pressure): pure macOS pressure level + spawn-gate verdict"
```

---

## Task 2: Config toggle (default ON) + max-agents

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"testing"
)

func TestSpawnGateDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_SPAWN_GATE", "")
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "")
	c := Load()
	if !c.SpawnGateEnabled {
		t.Error("spawn gate must default ON")
	}
	if c.SpawnGateMaxAgents != 5 {
		t.Errorf("max agents default = %d, want 5", c.SpawnGateMaxAgents)
	}
}

func TestSpawnGateDisable(t *testing.T) {
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("AGENTCTL_SPAWN_GATE", v)
		if Load().SpawnGateEnabled {
			t.Errorf("AGENTCTL_SPAWN_GATE=%q should disable the gate", v)
		}
	}
	t.Setenv("AGENTCTL_SPAWN_GATE", "1")
	if !Load().SpawnGateEnabled {
		t.Error("AGENTCTL_SPAWN_GATE=1 should enable the gate")
	}
}

func TestSpawnGateMaxAgentsOverride(t *testing.T) {
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "8")
	if Load().SpawnGateMaxAgents != 8 {
		t.Error("max agents override not applied")
	}
	t.Setenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS", "garbage")
	if Load().SpawnGateMaxAgents != 5 {
		t.Error("unparseable max agents should fall back to 5")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `c.SpawnGateEnabled undefined`.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add fields to `Config`:

```go
type Config struct {
	Addr               string
	DataDir            string
	ClaudeProjectsDir  string
	NotifyEnabled      bool
	ApprovalsEnabled   bool
	SpawnGateEnabled   bool
	SpawnGateMaxAgents int
}
```

Add helpers (note: this toggle defaults **ON** — only explicit off-values disable, the inverse of `notifyEnabled`/`approvalsEnabled`):

```go
// spawnGateEnabled reads AGENTCTL_SPAWN_GATE; ON by default (the gate is soft,
// never hard-blocks), disabled only for 0/off/false.
func spawnGateEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_SPAWN_GATE")) {
	case "0", "off", "false":
		return false
	}
	return true
}

// spawnGateMaxAgents reads AGENTCTL_SPAWN_GATE_MAX_AGENTS (default 5). The count
// co-trigger fires when this many agents are already live. Unparseable → 5.
func spawnGateMaxAgents() int {
	if v := os.Getenv("AGENTCTL_SPAWN_GATE_MAX_AGENTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 5
}
```

Add `"strconv"` to the imports. Wire into `Load()`:

```go
	return Config{
		Addr:               envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		DataDir:            envOr("AGENTCTL_DATA_DIR", defaultDataDir()),
		ClaudeProjectsDir:  envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:      notifyEnabled(),
		ApprovalsEnabled:   approvalsEnabled(),
		SpawnGateEnabled:   spawnGateEnabled(),
		SpawnGateMaxAgents: spawnGateMaxAgents(),
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): AGENTCTL_SPAWN_GATE (default on) + max-agents"
```

---

## Task 3: `lifecycle.MemoryPressure` exec helper

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/pressure_test.go` (new)

The lifecycle runs commands through `l.run.Run(ctx, dir, name string, args ...string) (string, error)`. Reuse the existing test fake Runner (see `internal/lifecycle/lifecycle_test.go` / `runner_test.go`) — the test below defines a minimal inline fake so it is self-contained.

- [ ] **Step 1: Write the failing test**

```go
package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/srajanpathak/agentctl/internal/pressure"
)

// prFakeRunner returns a canned output/error for any command.
type prFakeRunner struct {
	out string
	err error
}

func (f prFakeRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return f.out, f.err
}

func TestMemoryPressureParsesLevel(t *testing.T) {
	l := New(prFakeRunner{out: "2\n"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Warn {
		t.Fatalf("MemoryPressure = (%v,%v), want (warn,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnError(t *testing.T) {
	// Non-macOS / sysctl missing: command errors → degrade to Normal, no error.
	l := New(prFakeRunner{err: errors.New("exec: sysctl not found")})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on exec error = (%v,%v), want (normal,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnGarbage(t *testing.T) {
	l := New(prFakeRunner{out: "not-a-number"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on garbage = (%v,%v), want (normal,nil)", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestMemoryPressure`
Expected: FAIL — `l.MemoryPressure undefined`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/lifecycle/lifecycle.go` (near `GitNumstat`/`RunClaudeP`). Add `"github.com/srajanpathak/agentctl/internal/pressure"` to the imports.

```go
// MemoryPressure reads the macOS memory-pressure level via sysctl. Best-effort:
// on any error (sysctl missing on non-macOS, unparseable output) it degrades to
// pressure.Normal with no error, so the spawn gate falls back to count-only.
func (l *Lifecycle) MemoryPressure(ctx context.Context) (pressure.Level, error) {
	out, err := l.run.Run(ctx, "", "sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return pressure.Normal, nil
	}
	lvl, perr := pressure.ParseSysctl(out)
	if perr != nil {
		return pressure.Normal, nil
	}
	return lvl, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/lifecycle/ -run TestMemoryPressure`
Expected: PASS.

- [ ] **Step 5: Add `MemoryPressure` to the daemon `Lifecycle` interface + adapter**

In `internal/daemon/api.go`, add to the `Lifecycle` interface (alongside `GitNumstat`):

```go
	// MemoryPressure reads the current macOS memory-pressure level (Normal on
	// non-macOS / error). Used by the sampler loop and the spawn gate.
	MemoryPressure(ctx context.Context) (pressure.Level, error)
```

Add `"github.com/srajanpathak/agentctl/internal/pressure"` to `api.go` imports.

In `internal/daemon/lifecycle_adapter.go` (the adapter wrapping `*lifecycle.Lifecycle`), forward it. Find the adapter type (it implements the other forwarding methods like `GitNumstat`) and add:

```go
func (a *lifecycleAdapter) MemoryPressure(ctx context.Context) (pressure.Level, error) {
	return a.lc.MemoryPressure(ctx)
}
```

(Match the adapter's actual receiver name/type and add the `pressure` import. If the adapter forwards `GitNumstat` as `a.lc.GitNumstat(...)`, mirror that exactly.)

- [ ] **Step 6: Verify it builds**

Run: `go build ./...`
Expected: success.

- [ ] **Step 7: Commit**

```bash
git add internal/lifecycle/ internal/daemon/api.go internal/daemon/lifecycle_adapter.go
git commit -m "feat(lifecycle): MemoryPressure sysctl reader + daemon interface"
```

---

## Task 4: Daemon pressure cache, sampler loop, and `GET /pressure`

**Files:**
- Modify: `internal/daemon/api.go` (Server fields + `SetSpawnGate`)
- Create: `internal/daemon/pressure_routes.go` (sampler + handler)
- Modify: `internal/daemon/server.go` (start the sampler)
- Modify: `internal/daemon/lifecycle_routes.go` (register the route)
- Modify: `internal/cli/daemon.go` (wire config)
- Test: `internal/daemon/pressure_routes_test.go` (new)

- [ ] **Step 1: Add Server fields + `SetSpawnGate`**

In `internal/daemon/api.go`, add to the `Server` struct (and add `"sync"` and `pressure` imports):

```go
	// pressure caching for the spawn gate + GET /pressure. Sampled by a
	// background loop (sibling to the poller); read on the spawn hot path.
	pressMu      sync.RWMutex
	pressLevel   pressure.Level
	spawnGate    bool // AGENTCTL_SPAWN_GATE
	spawnGateMax int  // AGENTCTL_SPAWN_GATE_MAX_AGENTS
```

Add a setter near `SetExecutor`/`SetNarrator`:

```go
// SetSpawnGate configures the memory-pressure spawn gate. enabled=false leaves
// the gauge live but never warns on spawn. Initializes the cached level to
// Normal so reads before the first sample are safe.
func (s *Server) SetSpawnGate(enabled bool, maxAgents int) {
	s.pressMu.Lock()
	defer s.pressMu.Unlock()
	s.spawnGate = enabled
	s.spawnGateMax = maxAgents
	if s.pressLevel == 0 {
		s.pressLevel = pressure.Normal
	}
}
```

- [ ] **Step 2: Write the failing test**

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/pressure"
)

func TestHandlePressure(t *testing.T) {
	st := newMemStore(t) // reuse the daemon package's test store helper
	s := &Server{store: st, spawnGate: true, spawnGateMax: 5, pressLevel: pressure.Warn}

	req := httptest.NewRequest(http.MethodGet, "/pressure", nil)
	rec := httptest.NewRecorder()
	s.handlePressure(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp pressureResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Level != int(pressure.Warn) || resp.LevelName != "warn" {
		t.Errorf("level = %d/%s, want 2/warn", resp.Level, resp.LevelName)
	}
	if !resp.Elevated {
		t.Error("warn level should report elevated")
	}
	if !resp.GateEnabled || resp.MaxAgents != 5 {
		t.Errorf("gate flags wrong: enabled=%v max=%d", resp.GateEnabled, resp.MaxAgents)
	}
	_ = context.Background()
}
```

> If `newMemStore` does not exist in the daemon test package, use whatever store constructor the existing daemon tests use (grep `internal/daemon/*_test.go` for the store setup helper) — match the established pattern.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandlePressure`
Expected: FAIL — `s.handlePressure undefined` / `pressureResponse undefined`.

- [ ] **Step 4: Implement the sampler + handler**

Create `internal/daemon/pressure_routes.go`:

```go
package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/srajanpathak/agentctl/internal/pressure"
)

// pressureSampleInterval is how often the sampler refreshes the cached level.
// Cheap (one sysctl); kept short so the gauge and gate react quickly.
const pressureSampleInterval = 5 * time.Second

// pressureResponse is the body for GET /pressure (feeds the gauge + UI gating).
type pressureResponse struct {
	Level       int    `json:"level"`
	LevelName   string `json:"level_name"`
	AgentCount  int    `json:"agent_count"`
	MaxAgents   int    `json:"max_agents"`
	Elevated    bool   `json:"elevated"`
	GateEnabled bool   `json:"gate_enabled"`
}

// samplePressure refreshes the cached level once.
func (s *Server) samplePressure(ctx context.Context) {
	lvl, _ := s.life.MemoryPressure(ctx)
	s.pressMu.Lock()
	s.pressLevel = lvl
	s.pressMu.Unlock()
}

// runPressureSampler refreshes the cached level on a ticker until ctx is done.
func (s *Server) runPressureSampler(ctx context.Context) {
	s.samplePressure(ctx) // prime immediately
	t := time.NewTicker(pressureSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.samplePressure(ctx)
		}
	}
}

// liveAgentCount counts non-terminal sessions (reuses liveStatus).
func (s *Server) liveAgentCount(ctx context.Context) int {
	sessions, err := s.store.List(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, sess := range sessions {
		if liveStatus(sess.Status) {
			n++
		}
	}
	return n
}

// spawnVerdict reads the cached level + live count and evaluates the gate.
func (s *Server) spawnVerdict(ctx context.Context) pressure.Verdict {
	s.pressMu.RLock()
	lvl, max := s.pressLevel, s.spawnGateMax
	s.pressMu.RUnlock()
	return pressure.Evaluate(lvl, s.liveAgentCount(ctx), max)
}

func (s *Server) handlePressure(w http.ResponseWriter, r *http.Request) {
	s.pressMu.RLock()
	lvl, gate, max := s.pressLevel, s.spawnGate, s.spawnGateMax
	s.pressMu.RUnlock()
	if lvl == 0 {
		lvl = pressure.Normal
	}
	v := pressure.Evaluate(lvl, s.liveAgentCount(r.Context()), max)
	writeJSON(w, http.StatusOK, pressureResponse{
		Level:       int(lvl),
		LevelName:   lvl.String(),
		AgentCount:  v.AgentCount,
		MaxAgents:   max,
		Elevated:    v.Elevated,
		GateEnabled: gate,
	})
}
```

- [ ] **Step 5: Register the route + start the sampler**

In `internal/daemon/lifecycle_routes.go` `registerLifecycleRoutes`, add:

```go
	r.Get("/pressure", s.handlePressure)
```

In `internal/daemon/server.go` `ListenAndServe`, start the sampler beside the poller (after the poller goroutine block, using the same `runCtx`):

```go
	go s.runPressureSampler(runCtx)
```

- [ ] **Step 6: Wire config in `internal/cli/daemon.go`**

After `srv := daemon.NewServer(...)` (and the existing `SetExecutor`/`SetNarrator` calls), add:

```go
		srv.SetSpawnGate(cfg.SpawnGateEnabled, cfg.SpawnGateMaxAgents)
```

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/daemon/ -run TestHandlePressure && go build ./...`
Expected: PASS + build success.

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/ internal/cli/daemon.go
git commit -m "feat(daemon): pressure sampler loop + GET /pressure + gate wiring"
```

---

## Task 5: Gate `handleSpawn` (HTTP 428 + verdict)

**Files:**
- Modify: `internal/daemon/api.go` (`SpawnRequest.Force` + `confirmationResponse`)
- Modify: `internal/daemon/lifecycle_routes.go` (gate)
- Test: `internal/daemon/lifecycle_routes_test.go` (add cases; create if absent)

We use **HTTP 428 (Precondition Required)**, not 409 — `/spawn` already returns 409 for "session already exists", so 428 keeps the confirmation case unambiguous for the client.

- [ ] **Step 1: Add request/response types**

In `internal/daemon/api.go`: add `Force bool \`json:"force"\`` to `SpawnRequest`, and a response struct:

```go
// confirmationResponse is the 428 body when the spawn gate warns. The client
// turns confirmation_required into ErrConfirmationRequired so surfaces can
// re-spawn with force=true.
type confirmationResponse struct {
	ConfirmationRequired bool             `json:"confirmation_required"`
	Verdict              pressure.Verdict `json:"verdict"`
}
```

- [ ] **Step 2: Write the failing test**

```go
func TestHandleSpawnGateWarns(t *testing.T) {
	st := newMemStore(t)
	life := &fakeLifecycle{} // reuse the daemon test fake; SpawnJob/Spawn return a session
	s := &Server{store: st, life: life, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Normal}
	// Seed one live agent so count (1) >= max (1) → elevated.
	st.Insert(context.Background(), &store.Session{ID: "a1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	req := httptest.NewRequest(http.MethodPost, "/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", rec.Code)
	}
	var resp confirmationResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.ConfirmationRequired || !resp.Verdict.Elevated {
		t.Fatalf("expected confirmation_required + elevated verdict, got %+v", resp)
	}
}

func TestHandleSpawnGateForceBypasses(t *testing.T) {
	st := newMemStore(t)
	life := &fakeLifecycle{}
	s := &Server{store: st, life: life, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Critical}
	st.Insert(context.Background(), &store.Session{ID: "a1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir(), Force: true})
	req := httptest.NewRequest(http.MethodPost, "/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("force spawn status = %d, want 201", rec.Code)
	}
}

func TestHandleSpawnGateDisabledProceeds(t *testing.T) {
	st := newMemStore(t)
	life := &fakeLifecycle{}
	s := &Server{store: st, life: life, spawnGate: false, spawnGateMax: 1, pressLevel: pressure.Critical}
	st.Insert(context.Background(), &store.Session{ID: "a1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	req := httptest.NewRequest(http.MethodPost, "/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("gate-off status = %d, want 201", rec.Code)
	}
}
```

> Match the existing daemon test fakes: grep `internal/daemon/*_test.go` for the fake `Lifecycle` implementation (it must satisfy the interface incl. the new `MemoryPressure` — add a stub method returning `pressure.Normal, nil` to that fake). Use the package's store + `notify` setup pattern. Add imports: `bytes`, `context`, `encoding/json`, `net/http`, `net/http/httptest`, `store`, `pressure`.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandleSpawnGate`
Expected: FAIL — gate not implemented (returns 201, not 428).

- [ ] **Step 4: Implement the gate**

In `internal/daemon/lifecycle_routes.go` `handleSpawn`, insert the gate **after all validation, immediately before `sess, err := s.life.Spawn(...)`** (line ~80):

```go
	// Memory-pressure soft gate: when enabled and the caller hasn't forced,
	// warn (HTTP 428) instead of spawning onto a strained machine. The client
	// re-spawns with force=true to confirm. Pipelines bypass this (they spawn
	// in-process via SpawnJob, not through this handler).
	if s.spawnGate && !req.Force {
		if v := s.spawnVerdict(r.Context()); v.Elevated {
			writeJSON(w, http.StatusPreconditionRequired, confirmationResponse{
				ConfirmationRequired: true,
				Verdict:              v,
			})
			return
		}
	}
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/daemon/ && go build ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): soft spawn gate in handleSpawn (HTTP 428 + verdict)"
```

---

## Task 6: Go client — 428 → `ErrConfirmationRequired`, `Force`, `Pressure`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go` (add cases; create if absent)

- [ ] **Step 1: Write the failing test**

```go
func TestSpawnConfirmationRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionRequired)
		json.NewEncoder(w).Encode(map[string]any{
			"confirmation_required": true,
			"verdict": map[string]any{
				"elevated": true, "level": 2, "agent_count": 6,
				"max_agents": 5, "reason": "pressure: warn",
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.Spawn(context.Background(), SpawnParams{Prompt: "x", Cwd: "/tmp"})
	var cre *ErrConfirmationRequired
	if !errors.As(err, &cre) {
		t.Fatalf("want ErrConfirmationRequired, got %v", err)
	}
	if !cre.Verdict.Elevated || cre.Verdict.Reason != "pressure: warn" {
		t.Fatalf("verdict not carried: %+v", cre.Verdict)
	}
}

func TestPressure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"level": 2, "level_name": "warn", "agent_count": 3,
			"max_agents": 5, "elevated": false, "gate_enabled": true,
		})
	}))
	defer srv.Close()
	c := New(srv.URL)
	p, err := c.Pressure(context.Background())
	if err != nil || p.LevelName != "warn" || !p.GateEnabled {
		t.Fatalf("Pressure = (%+v,%v)", p, err)
	}
}
```

> Add imports as needed (`errors`, `encoding/json`, `net/http`, `net/http/httptest`, `context`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run "TestSpawnConfirmationRequired|TestPressure"`
Expected: FAIL — `ErrConfirmationRequired` / `c.Pressure` undefined.

- [ ] **Step 3: Capture the error body in `doT`**

The current `doT` decodes the error body into `{error}` and discards it. Change it to read the body once and stash the raw bytes on `StatusError` so callers can parse structured 4xx payloads. Update `StatusError`:

```go
type StatusError struct {
	Code int
	Msg  string
	Body []byte // raw response body (for structured 4xx payloads)
}
```

In `doT`, replace the `resp.StatusCode >= 400` block with:

```go
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg, Body: raw}
	}
```

- [ ] **Step 4: Add the typed error, `Force`, and the parse in `Spawn`**

```go
// ErrConfirmationRequired is returned by Spawn when the daemon's memory-pressure
// gate warns (HTTP 428). Retry Spawn with SpawnParams.Force = true to proceed.
type ErrConfirmationRequired struct {
	Verdict pressure.Verdict
}

func (e *ErrConfirmationRequired) Error() string {
	return "spawn gate: " + e.Verdict.Reason + " — re-run with force to spawn anyway"
}
```

Add `"github.com/srajanpathak/agentctl/internal/pressure"` to imports. Add `Force bool` to `SpawnParams`, include it in the body map, and translate the 428 in `Spawn`:

```go
func (c *Client) Spawn(ctx context.Context, p SpawnParams) (*store.Session, error) {
	var s store.Session
	body := map[string]any{
		"type": p.Type, "ticket": p.Ticket, "repo": p.Repo,
		"branch": p.Branch, "pr": p.PR, "worktree": p.Worktree,
		"prompt": p.Prompt, "cwd": p.Cwd, "supervised": p.Supervised,
		"force": p.Force,
	}
	if err := c.doT(ctx, longTimeout, http.MethodPost, "/spawn", body, &s); err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusPreconditionRequired {
			var cr struct {
				ConfirmationRequired bool             `json:"confirmation_required"`
				Verdict              pressure.Verdict `json:"verdict"`
			}
			if json.Unmarshal(se.Body, &cr) == nil && cr.ConfirmationRequired {
				return nil, &ErrConfirmationRequired{Verdict: cr.Verdict}
			}
		}
		return nil, err
	}
	return &s, nil
}
```

- [ ] **Step 5: Add `Client.Pressure`**

```go
// PressureStatus mirrors GET /pressure.
type PressureStatus struct {
	Level       int    `json:"level"`
	LevelName   string `json:"level_name"`
	AgentCount  int    `json:"agent_count"`
	MaxAgents   int    `json:"max_agents"`
	Elevated    bool   `json:"elevated"`
	GateEnabled bool   `json:"gate_enabled"`
}

func (c *Client) Pressure(ctx context.Context) (PressureStatus, error) {
	var p PressureStatus
	err := c.do(ctx, http.MethodGet, "/pressure", nil, &p)
	return p, err
}
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/client/ && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/client/
git commit -m "feat(client): 428→ErrConfirmationRequired, SpawnParams.Force, Pressure()"
```

---

## Task 7: CLI `start --force`

**Files:**
- Modify: `internal/cli/lifecycle.go`

The CLI does not do an interactive y/N prompt (keeps it testable); on a gate warning it prints the verdict and instructs `--force`. The richer confirm UX lives in web/TUI.

- [ ] **Step 1: Add the flag**

In `newStartCmd()` (internal/cli/lifecycle.go), register a flag:

```go
	cmd.Flags().Bool("force", false, "spawn even when the memory-pressure gate warns")
```

- [ ] **Step 2: Read it into SpawnParams**

Where `SpawnParams{...}` is built, read and set `Force`:

```go
	force, _ := cmd.Flags().GetBool("force")
	// ... SpawnParams{ ..., Force: force }
```

- [ ] **Step 3: Handle the typed error**

Where the result of `clientFor(cmd).Spawn(...)` is checked, special-case the gate:

```go
	sess, err := clientFor(cmd).Spawn(cmd.Context(), params)
	if err != nil {
		var cre *client.ErrConfirmationRequired
		if errors.As(err, &cre) {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
			return fmt.Errorf("spawn blocked by memory-pressure gate")
		}
		return err
	}
```

Add `"errors"` and `"github.com/srajanpathak/agentctl/internal/client"` to imports if not present.

- [ ] **Step 4: Build + manual check**

Run: `go build ./... && go vet ./internal/cli/`
Expected: success.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/lifecycle.go
git commit -m "feat(cli): start --force to bypass the memory-pressure gate"
```

---

## Task 8: MCP `spawn_agent` force arg

**Files:**
- Modify: `internal/mcp/server.go`

- [ ] **Step 1: Add `force` to the spawn args struct**

Find `spawnArgs` (the struct bound to `spawn_agent`) and add:

```go
	Force bool `json:"force,omitempty" jsonschema:"spawn even when the memory-pressure gate warns (default false)"`
```

- [ ] **Step 2: Pass it + handle the typed error**

In the `spawn_agent` handler, set `Force: a.Force` in `client.SpawnParams{...}`, then handle the gate before the generic error:

```go
		sess, err := s.cl.Spawn(ctx, client.SpawnParams{
			Type: a.Type, Ticket: a.Ticket, Repo: a.Repo,
			Branch: a.Branch, PR: a.PR, Worktree: a.Worktree,
			Prompt: a.Prompt, Cwd: cwd, Supervised: a.Supervised, Force: a.Force,
		})
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return textResult("memory-pressure gate: " + cre.Verdict.Reason +
					"\nRe-call spawn_agent with force=true to spawn anyway."), nil, nil
			}
			return textResult("error: " + err.Error()), nil, nil
		}
```

Add `"errors"` to imports if not present. Also mention `force` in the tool `Description` string.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/mcp/server.go
git commit -m "feat(mcp): spawn_agent force arg + gate confirmation text"
```

---

## Task 9: Pipeline executor advisory log

**Files:**
- Modify: `internal/daemon/executor.go`

Pipeline jobs spawn in-process via `e.life.SpawnJob` and never hit `handleSpawn`, so they are not gated (by design — a DAG must not deadlock). Add a non-blocking advisory log when pressure is elevated, so fan-out under pressure is at least visible.

- [ ] **Step 1: Add the advisory before `SpawnJob`**

In `executor.go`, just before `sess, serr := e.life.SpawnJob(ctx, req)` (around line 73), add:

```go
		if lvl, _ := e.life.MemoryPressure(ctx); lvl >= pressure.Warn {
			log.Printf("pipeline %s job %s: spawning under memory pressure (%s)", d.PipelineID, jobID, lvl)
		}
```

Add `"log"` and `"github.com/srajanpathak/agentctl/internal/pressure"` to imports. (Adjust `d.PipelineID`/`jobID` to the variables in scope at that point.)

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/executor.go
git commit -m "feat(executor): advisory log when a pipeline job spawns under pressure"
```

---

## Task 10: Web — confirm-anyway flow + pressure gauge

**Files:**
- Create: `web/src/lib/pressure.ts`
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/NewAgentModal.tsx`
- Modify: the fleet header component (locate: grep `web/src/components` for the stats/header that renders agent counts — likely `FleetStats.tsx` or the Overview header)

- [ ] **Step 1: Pure formatting helper**

`web/src/lib/pressure.ts`:

```ts
export interface PressureStatus {
  level: number;
  level_name: string;
  agent_count: number;
  max_agents: number;
  elevated: boolean;
  gate_enabled: boolean;
}

export interface Verdict {
  elevated: boolean;
  level: number;
  agent_count: number;
  max_agents: number;
  reason: string;
}

// gaugeClass maps a level to a CSS severity class for the gauge chip.
export function gaugeClass(level: number): 'ok' | 'warn' | 'crit' {
  if (level >= 4) return 'crit';
  if (level >= 2) return 'warn';
  return 'ok';
}

// gaugeLabel renders the always-on gauge text, e.g. "pressure: warn · 6 agents".
export function gaugeLabel(p: PressureStatus): string {
  return `pressure: ${p.level_name} · ${p.agent_count} agent${p.agent_count === 1 ? '' : 's'}`;
}
```

> If the web app has a test runner (check `web/package.json` for `vitest`/`jest`), add `web/src/lib/pressure.test.ts` asserting `gaugeClass(1)==='ok'`, `gaugeClass(2)==='warn'`, `gaugeClass(4)==='crit'`. If there is **no** web test runner, skip the test file (do not add a framework) and note it in the commit.

- [ ] **Step 2: API — force param, getPressure, 428 handling**

In `web/src/lib/api.ts`:

Add `force` to `SpawnParams` and the `spawn` body, and detect 428 before the generic `parse`:

```ts
export interface SpawnParams {
  // ...existing fields...
  force?: boolean;
}

export class ConfirmationRequiredError extends Error {
  constructor(public verdict: Verdict) {
    super(verdict.reason);
    this.name = 'ConfirmationRequiredError';
  }
}

export async function spawn(p: SpawnParams): Promise<Session> {
  const res = await fetch('/spawn', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      type: p.type ?? '', ticket: p.ticket ?? '', repo: p.repo ?? '',
      branch: p.branch ?? '', pr: p.pr ?? '', worktree: !!p.worktree,
      prompt: p.prompt ?? '', cwd: p.cwd ?? '', supervised: !!p.supervised,
      force: !!p.force,
    }),
  });
  if (res.status === 428) {
    const body = await res.json() as { verdict: Verdict };
    throw new ConfirmationRequiredError(body.verdict);
  }
  return parse<Session>(res);
}

export async function getPressure(): Promise<PressureStatus> {
  return parse<PressureStatus>(await fetch('/pressure'));
}
```

Add imports at top: `import type { Verdict, PressureStatus } from './pressure';`.

- [ ] **Step 3: NewAgentModal — confirm-anyway**

In `web/src/components/NewAgentModal.tsx`, add a verdict state and a two-step submit. Replace the `submit` + actions with:

```tsx
  const [confirm, setConfirm] = useState<string | null>(null);

  async function doSpawn(force: boolean) {
    setErr(null);
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
    if (!dir) { setErr('choose a directory to launch the agent from'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt, cwd: dir, supervised, force });
      onCreated(s.id);
    } catch (e) {
      if (e instanceof ConfirmationRequiredError) {
        setConfirm(e.verdict.reason);
      } else {
        setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
      }
    } finally {
      setBusy(false);
    }
  }
```

In the actions JSX, when `confirm` is set, show the warning + a "Spawn anyway" button:

```tsx
        {confirm && (
          <p className="warn">⚠ memory pressure: {confirm}. Spawn anyway?</p>
        )}
        <div className="actions">
          {confirm
            ? <button disabled={busy} onClick={() => doSpawn(true)}>Spawn anyway</button>
            : <button disabled={busy || !dir} onClick={() => doSpawn(false)}>Create</button>}
          <button onClick={onClose}>Cancel</button>
        </div>
```

Update the import: `import { spawn, ApiError, ConfirmationRequiredError } from '../lib/api';`. (Remove the old `submit` function and its `Cmd+Enter` handler call, or point the handler at `doSpawn(false)`.)

- [ ] **Step 4: Fleet header gauge**

In the fleet header/stats component, poll `getPressure()` (e.g. in a `useEffect` with a `setInterval` of ~5s, cleared on unmount) and render a chip:

```tsx
  const [press, setPress] = useState<PressureStatus | null>(null);
  useEffect(() => {
    let alive = true;
    const tick = () => getPressure().then(p => { if (alive) setPress(p); }).catch(() => {});
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);
  // ...in render:
  {press && <span className={`pressure-gauge ${gaugeClass(press.level)}`}>{gaugeLabel(press)}</span>}
```

Add imports for `getPressure`, `gaugeClass`, `gaugeLabel`, `PressureStatus`. Add minimal CSS for `.pressure-gauge.ok/.warn/.crit` in the existing stylesheet (green/amber/red text).

- [ ] **Step 5: Build the web bundle**

Run: `cd web && npm run build` (and `npm test` if a runner exists).
Expected: TypeScript compiles, bundle builds.

- [ ] **Step 6: Commit**

```bash
git add web/
git commit -m "feat(web): pressure gauge + spawn confirm-anyway dialog"
```

---

## Task 11: TUI — new-agent confirm + header gauge

**Files:**
- Modify: `internal/tui/*` (locate the new-agent submit + the header render)

The TUI new-agent flow (`n` key) currently calls the client `Spawn`. Locate it (grep `internal/tui` for `.Spawn(` and the new-agent input model — likely in `list_pane.go` / a `new_agent` model).

- [ ] **Step 1: Handle the gate on submit**

Where the TUI calls `Spawn` (likely inside a `tea.Cmd` that returns a result message), branch on `client.ErrConfirmationRequired`: store the verdict on the model and render a confirm line ("⚠ pressure: <reason> — press `f` to spawn anyway, `esc` to cancel"). On `f`, re-issue the spawn `tea.Cmd` with `Force: true`.

Concretely, in the spawn command:

```go
func spawnCmd(cl *client.Client, p client.SpawnParams) tea.Cmd {
	return func() tea.Msg {
		sess, err := cl.Spawn(context.Background(), p)
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return spawnGateMsg{verdict: cre.Verdict, params: p}
			}
			return spawnErrMsg{err}
		}
		return spawnOKMsg{sess}
	}
}
```

In `Update`, handle `spawnGateMsg` by storing `m.pendingGate = msg` and showing the confirm prompt; on `key.f` when `m.pendingGate` is set, run `spawnCmd(cl, withForce(m.pendingGate.params))` where `withForce` sets `Force = true`.

> Match the TUI's actual message/model types and key-handling style. Keep the confirm passive (no blocking) — same spirit as the approvals row.

- [ ] **Step 2: Header gauge**

Add a small periodic poll (`tea.Tick` every ~5s → a `tea.Cmd` calling `cl.Pressure`) that stores the latest `client.PressureStatus` on the model, and render `pressure: <name> · <n> agents` in the header (color by level using the TUI's existing lipgloss style palette). If a `tea.Tick` loop is awkward, piggyback the pressure fetch on the existing refresh cycle if one exists.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): new-agent pressure confirm + header gauge"
```

---

## Task 12: Full build, test, and integration

- [ ] **Step 1: Full Go test suite**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 2: Vet + web build**

Run: `go vet ./... && cd web && npm run build && cd ..`
Expected: clean.

- [ ] **Step 3: Release build (embeds web/dist)**

Run: `make release` (or the repo's documented build — check the Makefile target that embeds the web bundle).
Expected: binary builds.

- [ ] **Step 4: Commit any build artifacts / final touches**

```bash
git add -A
git commit -m "chore(spawn-gate): integrate memory-pressure gate end-to-end"
```

- [ ] **Step 5: Manual smoke (LEFT FOR USER — record results, don't claim verified)**

Reinstall the daemon (`make install` or the documented reinstall) so it serves `/pressure` + the new gate, then:
1. `curl -s localhost:8765/pressure` → returns level/agent_count/gate_enabled.
2. Set `AGENTCTL_SPAWN_GATE_MAX_AGENTS=1`, restart daemon, spawn with ≥1 live agent →
   - CLI `agentctl start "..."` prints the warning + `--force` hint; `--force` proceeds.
   - MCP `spawn_agent` returns the gate text; `force=true` proceeds.
   - Web New-agent shows "Spawn anyway"; clicking it spawns.
   - TUI `n` shows the confirm; `f` spawns.
   - Gauge visible in web + TUI headers.
3. `AGENTCTL_SPAWN_GATE=off`, restart → no warnings; gauge still shows.

---

## Self-Review Notes

- **Spec coverage:** signal (Task 1/3), trigger pressure OR count (Task 1 `Evaluate`), soft-warn-confirm (Tasks 5–11), non-interactive force flag (Tasks 6–8), pipeline advisory (Task 9), gauge (Tasks 4/10/11), toggle default-on (Task 2). All design sections map to a task.
- **Status 428 vs 409:** chosen deliberately to avoid collision with the existing 409 ("session already exists") on `/spawn`.
- **Type consistency:** `pressure.Verdict` JSON tags (`agent_count`, `max_agents`, `reason`, `elevated`, `level`) are identical across daemon `confirmationResponse`, Go client parse, and web `Verdict`. `PressureStatus`/`pressureResponse` fields match across Go and TS.
- **Gate placement:** the in-process executor bypasses `handleSpawn` (uses `SpawnJob`), so pipelines are advisory-only by construction — no deadlock risk.
- **YAGNI:** no crossing-notification, no per-agent RSS, no CPU/network, no history.
```
