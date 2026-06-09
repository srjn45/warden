# Warden Observability Surface — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a resource-first observability surface to warden — a live `GET /metrics` snapshot, an always-on JSONL recorder, and `GET /metrics/history` read-back — exposing per-agent RSS/CPU attribution, system memory pressure, and daemon self-stats, consumed via `warden stats` and a web Resources panel.

**Architecture:** A new pure-at-the-core `internal/metrics` package (parsers + collector + recorder), mirroring the `pressure`/`digest` pattern: all logic that can be wrong (process-tree aggregation, byte math, `etime` parsing) lives in pure functions tested against captured fixtures; the only impure part is a thin `Runner` that shells out to `ps`/`tmux`/`vm_stat`/`sysctl`. The daemon wires a `Collector` + `Recorder`, exposes two routes, starts the recorder goroutine beside the existing `runPressureSampler`, and gates the recorder behind a new `WARDEN_METRICS` flag. CLI (`warden stats`) and web (`ResourcesPanel`, uPlot chart) consume the endpoints.

**Tech Stack:** Go 1.26, go-chi router, cobra CLI; web is Astro + React 19 + vitest; uPlot for the history chart.

**Spec:** `docs/superpowers/specs/2026-06-09-warden-observability-design.md`

**Conventions in this codebase (read before starting):**
- Pure packages (`internal/pressure`, `internal/approval`, `internal/digest`) do no I/O; exec is injected. Follow that.
- The exec seam is `lifecycle.Runner` (`Run(ctx, dir, name string, args ...string) (string, error)`); `lifecycle.ExecRunner{}` is the real impl and `lifecycle.FakeRunner` (in `internal/lifecycle/runner.go`) is the test double. The metrics package defines its **own** structural `Runner` interface so it need not import `lifecycle`; `ExecRunner` and `FakeRunner` satisfy it automatically.
- Daemon HTTP helpers: `writeJSON(w, code, v)`, `writeErr(w, code, msg)` (in `internal/daemon/api.go`).
- Run a single Go test: `go test ./internal/metrics/ -run TestName -v`. Run a web test: `cd web && npx vitest run src/lib/metrics.test.ts`.
- Commit after each task. Use the `feat/observability` branch (already created; the spec commit is `3905d4e`).

---

## File Structure

**Create:**
- `internal/metrics/types.go` — wire types: `Sample`, `SystemStats`, `AgentStat`, `DaemonStat`, `Agent`.
- `internal/metrics/parse.go` — pure parsers: `parsePSTable`, `parseEtime`, `aggregateTree`, `parseVMStat`, `parseSwapUsed`, `parseMemSize`, `buildSystemStats`.
- `internal/metrics/parse_test.go`
- `internal/metrics/collect.go` — `Runner`, `Lister` interfaces; `Collector` + `Sample(ctx)`.
- `internal/metrics/collect_test.go`
- `internal/metrics/recorder.go` — `Recorder`: `Record`, `History`, `Prune`.
- `internal/metrics/recorder_test.go`
- `internal/daemon/metrics_routes.go` — Server metrics fields, `SetMetrics`, `handleMetrics`, `handleMetricsHistory`, `runMetricsRecorder`, `storeAgentLister`.
- `internal/daemon/metrics_routes_test.go`
- `internal/cli/stats.go` — `newStatsCmd` + pure `formatStats` helper.
- `internal/cli/stats_test.go`
- `web/src/lib/metrics.ts` — TS types + pure `historySeries` shaping helper + `fmtBytes`.
- `web/src/lib/metrics.test.ts`
- `web/src/components/ResourcesPanel.tsx`

**Modify:**
- `internal/config/config.go` — add `MetricsEnabled` field + `metricsEnabled()` helper.
- `internal/config/config_test.go` — cover the new flag.
- `internal/daemon/api.go` — register the two routes; add metrics fields to `Server`.
- `internal/daemon/server.go` — start `runMetricsRecorder` goroutine.
- `internal/client/client.go` — `MetricsSnapshot` types + `GetMetrics`, `GetMetricsHistory`.
- `internal/cli/root.go` — register `newStatsCmd()`.
- `internal/cli/daemon.go` — wire the collector + recorder via `srv.SetMetrics(...)`.
- `web/src/lib/api.ts` — `getMetrics`, `getMetricsHistory`.
- `web/src/components/OverviewTab.tsx` — add the Resources section.
- `web/package.json` — add `uplot` dependency.

---

## Task 1: Metrics wire types

**Files:**
- Create: `internal/metrics/types.go`

- [ ] **Step 1: Write the types file**

```go
// Package metrics samples warden's resource footprint: per-agent memory/CPU
// attribution, system memory pressure, and the daemon's own stats. The parsing
// and aggregation are pure (parse.go) so they're unit-testable; only collect.go
// shells out, through an injected Runner (mirrors internal/pressure + digest).
package metrics

import "time"

// Sample is one point-in-time snapshot. It crosses the wire (daemon → clients)
// and is the line format of the on-disk recorder.
type Sample struct {
	TakenAt time.Time   `json:"taken_at"`
	System  SystemStats `json:"system"`
	Agents  []AgentStat `json:"agents"`
	Daemon  DaemonStat  `json:"daemon"`
}

// SystemStats is machine-wide memory state.
type SystemStats struct {
	TotalBytes         uint64 `json:"total_bytes"`
	UsedBytes          uint64 `json:"used_bytes"`
	FreeBytes          uint64 `json:"free_bytes"`
	WiredBytes         uint64 `json:"wired_bytes"`
	CompressedBytes    uint64 `json:"compressed_bytes"`
	SwapUsedBytes      uint64 `json:"swap_used_bytes"`
	PressureLevel      string `json:"pressure_level"` // "normal" | "warn" | "critical"
	AgentCount         int    `json:"agent_count"`
	AttributedRSSBytes uint64 `json:"attributed_rss_bytes"`
}

// AgentStat is one agent's resource usage. Paneable is false when the tmux/ps
// lookup failed (dead pane, mid-teardown) — RSS/CPU are then zero.
type AgentStat struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	Paneable   bool    `json:"paneable"`
	RSSBytes   uint64  `json:"rss_bytes"`
	CPUPercent float64 `json:"cpu_percent"`
	ProcCount  int     `json:"proc_count"`
	UptimeSec  int64   `json:"uptime_sec"`
}

// DaemonStat is the daemon's own footprint. OpenFDs is best-effort (0 if it
// can't be read on this platform).
type DaemonStat struct {
	RSSBytes   uint64 `json:"rss_bytes"`
	Goroutines int    `json:"goroutines"`
	OpenFDs    int    `json:"open_fds"`
}

// Agent is the minimal session info the Collector needs from the store. The
// daemon adapts store.Session → Agent so this package stays store-free.
type Agent struct {
	ID          string
	TmuxSession string
	Status      string
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/metrics/`
Expected: success (no output).

- [ ] **Step 3: Commit**

```bash
git add internal/metrics/types.go
git commit -m "feat(metrics): wire types for resource sampling"
```

---

## Task 2: Pure `ps` table parser + etime + tree aggregation

**Files:**
- Create: `internal/metrics/parse.go`
- Test: `internal/metrics/parse_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package metrics

import "testing"

func TestParseEtime(t *testing.T) {
	cases := map[string]int64{
		"05:09":       309,        // mm:ss
		"01:02:03":    3723,       // hh:mm:ss
		"2-03:04:05":  183845,     // dd-hh:mm:ss
		"00:00":       0,
	}
	for in, want := range cases {
		if got := parseEtime(in); got != want {
			t.Fatalf("parseEtime(%q)=%d want %d", in, got, want)
		}
	}
}

func TestParsePSTable(t *testing.T) {
	// Columns: pid ppid rss(KiB) pcpu etime — output of
	// `ps -axo pid=,ppid=,rss=,pcpu=,etime=`.
	raw := "  100     1  20480   1.5    05:09\n" +
		"  200   100  51200  12.0 01:02:03\n" +
		"  300   200   1024   0.0    00:30\n"
	tbl := parsePSTable(raw)
	if len(tbl) != 3 {
		t.Fatalf("rows=%d want 3", len(tbl))
	}
	r := tbl[200]
	if r.PPID != 100 || r.RSSKiB != 51200 || r.CPU != 12.0 || r.EtimeSec != 3723 {
		t.Fatalf("row 200 = %+v", r)
	}
}

func TestAggregateTree(t *testing.T) {
	raw := "  100     1  20480   1.5    05:09\n" +
		"  200   100  51200  12.0    05:00\n" + // child of 100
		"  300   200   1024   0.5    04:00\n" + // grandchild
		"  900     1   8000   0.0    00:10\n"   // unrelated
	tbl := parsePSTable(raw)
	rss, cpu, procs, uptime := aggregateTree(tbl, []int{100})
	if rss != (20480+51200+1024)*1024 {
		t.Fatalf("rss=%d", rss)
	}
	if cpu != 14.0 || procs != 3 || uptime != 309 {
		t.Fatalf("cpu=%v procs=%d uptime=%d", cpu, procs, uptime)
	}
}

func TestAggregateTreeMissingRoot(t *testing.T) {
	tbl := parsePSTable("  100   1  2048  0.0  00:05\n")
	rss, _, procs, _ := aggregateTree(tbl, []int{999})
	if rss != 0 || procs != 0 {
		t.Fatalf("missing root should aggregate to zero, got rss=%d procs=%d", rss, procs)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/metrics/ -run 'TestParse|TestAggregate' -v`
Expected: FAIL — `undefined: parseEtime` etc.

- [ ] **Step 3: Implement the parsers**

```go
package metrics

import (
	"strconv"
	"strings"
)

// ProcRow is one row of the ps table.
type ProcRow struct {
	PID      int
	PPID     int
	RSSKiB   uint64
	CPU      float64
	EtimeSec int64
}

// parseEtime parses BSD ps elapsed time: "[[dd-]hh:]mm:ss" → seconds.
func parseEtime(s string) int64 {
	s = strings.TrimSpace(s)
	var days int64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.ParseInt(s[:i], 10, 64)
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m, sec int64
	switch len(parts) {
	case 3:
		h, _ = strconv.ParseInt(parts[0], 10, 64)
		m, _ = strconv.ParseInt(parts[1], 10, 64)
		sec, _ = strconv.ParseInt(parts[2], 10, 64)
	case 2:
		m, _ = strconv.ParseInt(parts[0], 10, 64)
		sec, _ = strconv.ParseInt(parts[1], 10, 64)
	default:
		return 0
	}
	return days*86400 + h*3600 + m*60 + sec
}

// parsePSTable parses `ps -axo pid=,ppid=,rss=,pcpu=,etime=` output (5
// whitespace-separated columns; etime has no internal spaces) into a pid→row
// map. Malformed rows are skipped.
func parsePSTable(raw string) map[int]ProcRow {
	out := make(map[int]ProcRow)
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseUint(f[2], 10, 64)
		cpu, _ := strconv.ParseFloat(f[3], 64)
		out[pid] = ProcRow{PID: pid, PPID: ppid, RSSKiB: rss, CPU: cpu, EtimeSec: parseEtime(f[4])}
	}
	return out
}

// aggregateTree sums RSS (→ bytes), CPU%, process count, and the oldest root's
// uptime over each root pid and all its descendants. Roots absent from the table
// contribute nothing. RSS is returned in BYTES (ps reports KiB).
func aggregateTree(tbl map[int]ProcRow, roots []int) (rssBytes uint64, cpu float64, procs int, uptimeSec int64) {
	children := make(map[int][]int, len(tbl))
	for pid, r := range tbl {
		children[r.PPID] = append(children[r.PPID], pid)
	}
	visited := make(map[int]bool)
	var walk func(pid int)
	walk = func(pid int) {
		if visited[pid] {
			return
		}
		r, ok := tbl[pid]
		if !ok {
			return
		}
		visited[pid] = true
		rssBytes += r.RSSKiB * 1024
		cpu += r.CPU
		procs++
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for _, root := range roots {
		if r, ok := tbl[root]; ok && r.EtimeSec > uptimeSec {
			uptimeSec = r.EtimeSec
		}
		walk(root)
	}
	return rssBytes, cpu, procs, uptimeSec
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/metrics/ -run 'TestParse|TestAggregate' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/parse.go internal/metrics/parse_test.go
git commit -m "feat(metrics): pure ps-table parser, etime, process-tree aggregation"
```

---

## Task 3: Pure system-memory parsers (`vm_stat`, swap, memsize)

**Files:**
- Modify: `internal/metrics/parse.go`
- Test: `internal/metrics/parse_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestParseVMStat(t *testing.T) {
	raw := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free:                          100.\n" +
		"Pages active:                        200.\n" +
		"Pages inactive:                       50.\n" +
		"Pages wired down:                     30.\n" +
		"Pages occupied by compressor:         40.\n"
	pageSize, counts := parseVMStat(raw)
	if pageSize != 16384 {
		t.Fatalf("pageSize=%d", pageSize)
	}
	if counts["Pages free"] != 100 || counts["Pages occupied by compressor"] != 40 {
		t.Fatalf("counts=%+v", counts)
	}
}

func TestParseSwapUsed(t *testing.T) {
	raw := "vm.swapusage: total = 2048.00M  used = 512.50M  free = 1535.50M  (encrypted)"
	got := parseSwapUsed(raw)
	want := uint64(512.5 * 1024 * 1024)
	if got != want {
		t.Fatalf("swap used=%d want %d", got, want)
	}
}

func TestParseMemSize(t *testing.T) {
	if got := parseMemSize("17179869184\n"); got != 17179869184 {
		t.Fatalf("memsize=%d", got)
	}
	if got := parseMemSize("hw.memsize: 17179869184"); got != 17179869184 {
		t.Fatalf("memsize with prefix=%d", got)
	}
}

func TestBuildSystemStats(t *testing.T) {
	counts := map[string]int64{
		"Pages free":                    100,
		"Pages wired down":              30,
		"Pages occupied by compressor":  40,
	}
	ss := buildSystemStats(16384, counts, 1<<24 /*16MiB total*/, 1024*1024, "warn")
	if ss.FreeBytes != 100*16384 || ss.WiredBytes != 30*16384 || ss.CompressedBytes != 40*16384 {
		t.Fatalf("ss=%+v", ss)
	}
	if ss.UsedBytes != ss.TotalBytes-ss.FreeBytes || ss.TotalBytes != 1<<24 {
		t.Fatalf("used/total wrong: %+v", ss)
	}
	if ss.SwapUsedBytes != 1024*1024 || ss.PressureLevel != "warn" {
		t.Fatalf("swap/pressure wrong: %+v", ss)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/metrics/ -run 'TestParseVMStat|TestParseSwap|TestParseMemSize|TestBuildSystem' -v`
Expected: FAIL — `undefined: parseVMStat` etc.

- [ ] **Step 3: Implement the parsers (append to `parse.go`)**

```go
import "strings" // already imported; do not duplicate

// parseVMStat parses `vm_stat` output: the page size from the header and each
// "Key: N." line into a counts map (keyed by the text before the colon).
func parseVMStat(raw string) (pageSize int64, counts map[string]int64) {
	counts = make(map[string]int64)
	pageSize = 4096 // sane default if the header is missing
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				rest = strings.TrimSuffix(strings.TrimSpace(rest), " bytes)")
				if n, err := strconv.ParseInt(strings.Fields(rest)[0], 10, 64); err == nil {
					pageSize = n
				}
			}
			continue
		}
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[i+1:]), "."))
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			counts[key] = n
		}
	}
	return pageSize, counts
}

// parseSwapUsed extracts the "used = N.NNM" figure from `sysctl vm.swapusage`
// and returns it in bytes (suffix M=MiB, G=GiB, K=KiB).
func parseSwapUsed(raw string) uint64 {
	i := strings.Index(raw, "used =")
	if i < 0 {
		return 0
	}
	f := strings.Fields(raw[i+len("used ="):])
	if len(f) == 0 {
		return 0
	}
	tok := f[0]
	mult := 1.0
	switch {
	case strings.HasSuffix(tok, "G"):
		mult, tok = 1<<30, strings.TrimSuffix(tok, "G")
	case strings.HasSuffix(tok, "M"):
		mult, tok = 1<<20, strings.TrimSuffix(tok, "M")
	case strings.HasSuffix(tok, "K"):
		mult, tok = 1<<10, strings.TrimSuffix(tok, "K")
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return 0
	}
	return uint64(v * mult)
}

// parseMemSize parses `sysctl -n hw.memsize` (bare value or "hw.memsize: N").
func parseMemSize(raw string) uint64 {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	v, _ := strconv.ParseUint(strings.Fields(s+" ")[0], 10, 64)
	return v
}

// buildSystemStats assembles SystemStats from parsed pieces. UsedBytes is
// total-free (guarded against underflow). Agent count/attributed RSS are filled
// by the Collector, not here.
func buildSystemStats(pageSize int64, counts map[string]int64, total, swapUsed uint64, pressure string) SystemStats {
	px := uint64(pageSize)
	free := uint64(counts["Pages free"]) * px
	wired := uint64(counts["Pages wired down"]) * px
	compressed := uint64(counts["Pages occupied by compressor"]) * px
	used := uint64(0)
	if total > free {
		used = total - free
	}
	return SystemStats{
		TotalBytes:      total,
		UsedBytes:       used,
		FreeBytes:       free,
		WiredBytes:      wired,
		CompressedBytes: compressed,
		SwapUsedBytes:   swapUsed,
		PressureLevel:   pressure,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/metrics/ -run 'TestParseVMStat|TestParseSwap|TestParseMemSize|TestBuildSystem' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/parse.go internal/metrics/parse_test.go
git commit -m "feat(metrics): pure vm_stat/swap/memsize parsers + system-stats builder"
```

---

## Task 4: Collector — orchestrate exec into a Sample

**Files:**
- Create: `internal/metrics/collect.go`
- Test: `internal/metrics/collect_test.go`

- [ ] **Step 1: Write the failing test**

```go
package metrics

import (
	"context"
	"testing"
)

// fakeRunner returns canned output per "name arg0 arg1 ..." key, like
// lifecycle.FakeRunner but local so this package needn't import lifecycle.
type fakeRunner struct{ resp map[string]string }

func (f fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	return f.resp[key], nil
}

type fakeLister struct{ agents []Agent }

func (f fakeLister) LiveAgents(_ context.Context) ([]Agent, error) { return f.agents, nil }

func TestCollectorSample(t *testing.T) {
	ps := "  100     1  20480   1.0    10:00\n" + // agent A pane (pid 100)
		"  101   100  51200   5.0    09:00\n" + // claude child of A
		"  200     1  10240   0.5    05:00\n" + // agent B pane (pid 200)
		"  999     1  30000   0.0    01:00\n"   // the daemon itself (self pid)
	c := &Collector{
		Run: fakeRunner{resp: map[string]string{
			"ps -axo pid=,ppid=,rss=,pcpu=,etime=":           ps,
			"tmux list-panes -F #{pane_pid} -t agent-a":       "100\n",
			"tmux list-panes -F #{pane_pid} -t agent-b":       "200\n",
			"vm_stat":                                          "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 100.\n",
			"sysctl -n hw.memsize":                             "17179869184\n",
			"sysctl -n vm.swapusage":                           "vm.swapusage: total = 2048.00M used = 256.00M free = 1792.00M",
		}}.Run,
		Lister:    fakeLister{agents: []Agent{{ID: "agent-a", TmuxSession: "agent-a", Status: "working"}, {ID: "agent-b", TmuxSession: "agent-b", Status: "idle"}}},
		SelfPID:   999,
		Pressure:  func() string { return "warn" },
	}
	s, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 2 {
		t.Fatalf("agents=%d want 2", len(s.Agents))
	}
	a := s.Agents[0] // agent-a: pane 100 + child 101
	if !a.Paneable || a.RSSBytes != (20480+51200)*1024 || a.ProcCount != 2 || a.CPUPercent != 6.0 {
		t.Fatalf("agent-a = %+v", a)
	}
	if s.System.AgentCount != 2 || s.System.AttributedRSSBytes != (20480+51200+10240)*1024 {
		t.Fatalf("system totals wrong: %+v", s.System)
	}
	if s.System.PressureLevel != "warn" || s.System.TotalBytes != 17179869184 {
		t.Fatalf("system mem wrong: %+v", s.System)
	}
	if s.Daemon.RSSBytes != 30000*1024 || s.Daemon.Goroutines < 1 {
		t.Fatalf("daemon stats wrong: %+v", s.Daemon)
	}
}

func TestCollectorAgentDegradesWhenPaneMissing(t *testing.T) {
	c := &Collector{
		Run: fakeRunner{resp: map[string]string{
			"ps -axo pid=,ppid=,rss=,pcpu=,etime=": "  1  0  100  0.0  00:01\n",
			// no tmux list-panes entry → empty pane list → non-paneable
		}}.Run,
		Lister:   fakeLister{agents: []Agent{{ID: "ghost", TmuxSession: "ghost", Status: "orphaned"}}},
		SelfPID:  1,
		Pressure: func() string { return "normal" },
	}
	s, err := c.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 1 || s.Agents[0].Paneable || s.Agents[0].RSSBytes != 0 {
		t.Fatalf("ghost agent should be non-paneable zero: %+v", s.Agents)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run TestCollector -v`
Expected: FAIL — `undefined: Collector`.

- [ ] **Step 3: Implement the Collector**

```go
package metrics

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Runner shells out a command and returns combined output. lifecycle.ExecRunner
// and lifecycle.FakeRunner satisfy it structurally, so this package needn't
// import lifecycle.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

// Lister supplies the live agents to attribute. The daemon adapts store.Session.
type Lister interface {
	LiveAgents(ctx context.Context) ([]Agent, error)
}

// Collector turns one ps scan + per-agent tmux lookups + system sysctls into a
// Sample. All fields are injectable so it's unit-testable with no real procs.
type Collector struct {
	// Run executes ps/tmux/vm_stat/sysctl. Required.
	Run func(ctx context.Context, dir, name string, args ...string) (string, error)
	// Lister returns the agents to attribute. Required.
	Lister Lister
	// SelfPID is the daemon's pid for self-stats; 0 ⇒ os.Getpid().
	SelfPID int
	// Pressure returns the cached pressure level name ("normal"/"warn"/...).
	// nil ⇒ "normal".
	Pressure func() string
	// now is injectable for tests; nil ⇒ time.Now.
	now func() time.Time
}

// NewCollector builds a Collector from a Runner and Lister (the daemon's wiring
// path). SelfPID defaults to the current process.
func NewCollector(run Runner, lister Lister, pressure func() string) *Collector {
	return &Collector{Run: run.Run, Lister: lister, SelfPID: os.Getpid(), Pressure: pressure}
}

func (c *Collector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Sample collects one snapshot. It is best-effort: a failing sub-command degrades
// the affected field (agent → non-paneable; system field → 0) but never aborts.
func (c *Collector) Sample(ctx context.Context) (Sample, error) {
	psOut, _ := c.Run(ctx, "", "ps", "-axo", "pid=,ppid=,rss=,pcpu=,etime=")
	tbl := parsePSTable(psOut)

	agents, err := c.Lister.LiveAgents(ctx)
	if err != nil {
		return Sample{}, err
	}

	out := Sample{TakenAt: c.clock()}
	var attributed uint64
	for _, ag := range agents {
		st := AgentStat{ID: ag.ID, Status: ag.Status}
		pids := c.panePIDs(ctx, ag.TmuxSession)
		if len(pids) > 0 {
			rss, cpu, procs, uptime := aggregateTree(tbl, pids)
			st.Paneable = procs > 0
			st.RSSBytes, st.CPUPercent, st.ProcCount, st.UptimeSec = rss, cpu, procs, uptime
			attributed += rss
		}
		out.Agents = append(out.Agents, st)
	}

	out.System = c.systemStats(ctx)
	out.System.AgentCount = len(agents)
	out.System.AttributedRSSBytes = attributed
	out.Daemon = c.daemonStats(tbl)
	return out, nil
}

// panePIDs resolves a tmux session's pane pids (one per pane). Empty on any
// failure (dead/unknown session).
func (c *Collector) panePIDs(ctx context.Context, session string) []int {
	if session == "" {
		return nil
	}
	out, err := c.Run(ctx, "", "tmux", "list-panes", "-F", "#{pane_pid}", "-t", session)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(out) {
		if n, err := strconv.Atoi(line); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

func (c *Collector) systemStats(ctx context.Context) SystemStats {
	vmOut, _ := c.Run(ctx, "", "vm_stat")
	pageSize, counts := parseVMStat(vmOut)
	memOut, _ := c.Run(ctx, "", "sysctl", "-n", "hw.memsize")
	swapOut, _ := c.Run(ctx, "", "sysctl", "-n", "vm.swapusage")
	level := "normal"
	if c.Pressure != nil {
		if l := c.Pressure(); l != "" {
			level = l
		}
	}
	return buildSystemStats(pageSize, counts, parseMemSize(memOut), parseSwapUsed(swapOut), level)
}

func (c *Collector) daemonStats(tbl map[int]ProcRow) DaemonStat {
	pid := c.SelfPID
	if pid == 0 {
		pid = os.Getpid()
	}
	d := DaemonStat{Goroutines: runtime.NumGoroutine(), OpenFDs: countOpenFDs()}
	if r, ok := tbl[pid]; ok {
		d.RSSBytes = r.RSSKiB * 1024
	}
	return d
}

// countOpenFDs counts the daemon's open file descriptors (best-effort; 0 if the
// platform doesn't expose /dev/fd).
func countOpenFDs() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/metrics/ -run TestCollector -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/collect.go internal/metrics/collect_test.go
git commit -m "feat(metrics): Collector assembling a Sample from ps/tmux/sysctl"
```

---

## Task 5: Recorder — append, rotate, prune, history read-back

**Files:**
- Create: `internal/metrics/recorder.go`
- Test: `internal/metrics/recorder_test.go`

- [ ] **Step 1: Write the failing test**

```go
package metrics

import (
	"context"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func TestRecorderRecordAndHistory(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Two samples on day 1, one on day 2.
	must := func(e error) { if e != nil { t.Fatal(e) } }
	must(r.Record(Sample{TakenAt: ts("2026-06-08T10:00:00Z"), System: SystemStats{AgentCount: 1}}))
	must(r.Record(Sample{TakenAt: ts("2026-06-08T10:00:15Z"), System: SystemStats{AgentCount: 2}}))
	must(r.Record(Sample{TakenAt: ts("2026-06-09T09:00:00Z"), System: SystemStats{AgentCount: 3}}))

	// History since the second sample → 2 newest-first.
	got, err := r.History(ts("2026-06-08T10:00:15Z"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].System.AgentCount != 3 || got[1].System.AgentCount != 2 {
		t.Fatalf("history=%+v", got)
	}

	// limit caps the result (newest kept).
	got, _ = r.History(time.Time{}, 1)
	if len(got) != 1 || got[0].System.AgentCount != 3 {
		t.Fatalf("limited history=%+v", got)
	}
}

func TestRecorderPrune(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRecorder(dir)
	_ = r.Record(Sample{TakenAt: ts("2026-05-01T10:00:00Z")}) // old
	_ = r.Record(Sample{TakenAt: ts("2026-06-09T10:00:00Z")}) // recent
	// keep 7 days relative to now=2026-06-09 → the May file is pruned.
	if err := r.Prune(ts("2026-06-09T12:00:00Z"), 7); err != nil {
		t.Fatal(err)
	}
	got, _ := r.History(time.Time{}, 100)
	if len(got) != 1 || got[0].TakenAt.Day() != 9 {
		t.Fatalf("after prune=%+v", got)
	}
}

func TestRecorderHistoryEmptyDir(t *testing.T) {
	r, _ := NewRecorder(t.TempDir())
	got, err := r.History(time.Time{}, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty history err=%v got=%+v", err, got)
	}
	_ = context.Background()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run TestRecorder -v`
Expected: FAIL — `undefined: NewRecorder`.

- [ ] **Step 3: Implement the Recorder**

```go
package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recorder appends Samples to per-day JSONL files under dir and reads them back.
// Append-only; one Sample per line. Safe for concurrent Record calls.
type Recorder struct {
	dir string
	mu  sync.Mutex
}

// NewRecorder ensures dir exists (0o700 — samples carry agent ids/host memory
// state, same sensitivity class as session files).
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Recorder{dir: dir}, nil
}

func (r *Recorder) fileFor(t time.Time) string {
	return filepath.Join(r.dir, t.UTC().Format("2006-01-02")+".jsonl")
}

// Record appends one sample to its day-file (chosen from sample.TakenAt).
func (r *Recorder) Record(s Sample) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.fileFor(s.TakenAt), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// History returns samples with TakenAt >= since, newest-first, capped at limit
// (<=0 ⇒ no cap). Missing/unreadable files and malformed lines are skipped, not
// errored.
func (r *Recorder) History(since time.Time, limit int) ([]Sample, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Sample
	for _, fp := range files {
		f, err := os.Open(fp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var s Sample
			if json.Unmarshal(sc.Bytes(), &s) != nil {
				continue
			}
			if !since.IsZero() && s.TakenAt.Before(since) {
				continue
			}
			out = append(out, s)
		}
		f.Close()
	}
	// newest-first
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Prune deletes day-files older than keepDays relative to now (by filename date).
func (r *Recorder) Prune(now time.Time, keepDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.UTC().AddDate(0, 0, -keepDays)
	files, err := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	if err != nil {
		return err
	}
	for _, fp := range files {
		base := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
		day, err := time.Parse("2006-01-02", base)
		if err != nil {
			continue
		}
		if day.Before(cutoff.Truncate(24 * time.Hour)) {
			_ = os.Remove(fp)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/metrics/ -run TestRecorder -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the whole package + commit**

Run: `go test ./internal/metrics/`
Expected: `ok  github.com/srjn45/warden/internal/metrics`

```bash
git add internal/metrics/recorder.go internal/metrics/recorder_test.go
git commit -m "feat(metrics): JSONL recorder with day-rotation, prune, history read-back"
```

---

## Task 6: Config flag `WARDEN_METRICS`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test (append to config_test.go)**

```go
func TestMetricsEnabledDefaultsOn(t *testing.T) {
	t.Setenv("WARDEN_METRICS", "")
	t.Setenv("AGENTCTL_METRICS", "")
	if !Load().MetricsEnabled {
		t.Fatal("metrics should default ON")
	}
}

func TestMetricsEnabledOff(t *testing.T) {
	t.Setenv("WARDEN_METRICS", "off")
	if Load().MetricsEnabled {
		t.Fatal("WARDEN_METRICS=off should disable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestMetricsEnabled -v`
Expected: FAIL — `MetricsEnabled` undefined.

- [ ] **Step 3: Implement**

Add the field to the `Config` struct (after `SpawnGateMaxAgents int`):

```go
	SpawnGateMaxAgents int
	MetricsEnabled     bool
```

Add the helper (after `spawnGateMaxAgents()`):

```go
// metricsEnabled reads WARDEN_METRICS (legacy AGENTCTL_METRICS); ON by default
// (the recorder is cheap and must run before a freeze to capture it), disabled
// only for 0/off/false.
func metricsEnabled() bool {
	switch strings.ToLower(env("METRICS")) {
	case "0", "off", "false":
		return false
	}
	return true
}
```

Wire it in `Load()` (add the field to the returned literal):

```go
		SpawnGateMaxAgents: spawnGateMaxAgents(),
		MetricsEnabled:     metricsEnabled(),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestMetricsEnabled -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): WARDEN_METRICS flag (on by default)"
```

---

## Task 7: Daemon routes + recorder goroutine + store adapter

**Files:**
- Create: `internal/daemon/metrics_routes.go`
- Test: `internal/daemon/metrics_routes_test.go`
- Modify: `internal/daemon/api.go` (Server fields + route registration)
- Modify: `internal/daemon/server.go` (start recorder)

- [ ] **Step 1: Add metrics fields to the `Server` struct**

In `internal/daemon/api.go`, add to the `Server` struct (after the `spawnGateMax int` field, before the closing brace):

```go
	spawnGateMax int  // WARDEN_SPAWN_GATE_MAX_AGENTS
	// metrics collection (resource observability). nil collector ⇒ /metrics
	// returns an empty sample; nil recorder ⇒ no on-disk recording.
	mcollector  *metrics.Collector
	mrecorder   *metrics.Recorder
	metricsOn   bool // WARDEN_METRICS — gates the disk recorder goroutine
```

Add the import to `api.go`'s import block:

```go
	"github.com/srjn45/warden/internal/metrics"
```

- [ ] **Step 2: Register the routes**

In `api.go`'s `router()`, add after the `s.handleDigest` line:

```go
	r.Get("/metrics", s.handleMetrics)
	r.Get("/metrics/history", s.handleMetricsHistory)
```

- [ ] **Step 3: Write the failing route test**

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/metrics"
)

func TestHandleMetricsLive(t *testing.T) {
	s := &Server{}
	s.mcollector = &metrics.Collector{
		Run: func(_ context.Context, _ string, name string, args ...string) (string, error) {
			if name == "ps" {
				return "  1  0  1024  0.0  00:05\n", nil
			}
			return "", nil
		},
		Lister:   staticLister{},
		SelfPID:  1,
		Pressure: func() string { return "normal" },
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got metrics.Sample
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Daemon.RSSBytes != 1024*1024 {
		t.Fatalf("daemon rss=%d", got.Daemon.RSSBytes)
	}
}

func TestHandleMetricsHistory(t *testing.T) {
	dir := t.TempDir()
	r, _ := metrics.NewRecorder(dir)
	_ = r.Record(metrics.Sample{TakenAt: time.Now(), System: metrics.SystemStats{AgentCount: 7}})
	s := &Server{mrecorder: r}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics/history?limit=10", nil)
	s.handleMetricsHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Samples) != 1 || resp.Samples[0].System.AgentCount != 7 {
		t.Fatalf("history=%+v", resp.Samples)
	}
}

func TestHandleMetricsHistoryNoRecorder(t *testing.T) {
	s := &Server{} // recorder nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics/history", nil)
	s.handleMetricsHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

type staticLister struct{}

func (staticLister) LiveAgents(_ context.Context) ([]metrics.Agent, error) { return nil, nil }
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandleMetrics -v`
Expected: FAIL — `s.handleMetrics` undefined.

- [ ] **Step 5: Implement `metrics_routes.go`**

```go
package daemon

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/store"
)

const (
	// metricsRecordInterval is how often the recorder samples to disk. Cheap
	// (one ps + a few sysctls) and frequent enough to catch a fast memory ramp.
	metricsRecordInterval = 15 * time.Second
	// metricsRetentionDays bounds the on-disk JSONL history.
	metricsRetentionDays = 7
	// metricsHistoryDefaultWindow is the default look-back when no `since` is given.
	metricsHistoryDefaultWindow = 2 * time.Hour
	// metricsHistoryMaxSamples caps a single history response.
	metricsHistoryMaxSamples = 1000
)

// SetMetrics wires the collector + recorder after construction. recorder may be
// nil (recording disabled); collector may be nil (live snapshot returns empty).
func (s *Server) SetMetrics(c *metrics.Collector, r *metrics.Recorder, enabled bool) {
	s.mcollector = c
	s.mrecorder = r
	s.metricsOn = enabled
}

// pressureName returns the cached pressure level name for the collector.
func (s *Server) pressureName() string {
	s.pressMu.RLock()
	lvl := s.pressLevel
	s.pressMu.RUnlock()
	if lvl == 0 {
		return "normal"
	}
	return lvl.String()
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.mcollector == nil {
		writeJSON(w, http.StatusOK, metrics.Sample{})
		return
	}
	sample, err := s.mcollector.Sample(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sample)
}

func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	type historyResponse struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if s.mrecorder == nil {
		writeJSON(w, http.StatusOK, historyResponse{Samples: []metrics.Sample{}})
		return
	}
	since := time.Now().Add(-metricsHistoryDefaultWindow)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	limit := metricsHistoryMaxSamples
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < limit {
			limit = n
		}
	}
	samples, err := s.mrecorder.History(since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if samples == nil {
		samples = []metrics.Sample{}
	}
	writeJSON(w, http.StatusOK, historyResponse{Samples: samples})
}

// runMetricsRecorder samples to disk on a ticker until ctx is done. Best-effort:
// each tick is panic-guarded so a sampling bug can't take down the daemon, and a
// daily prune trims old day-files. No-op when recording is disabled or unwired.
func (s *Server) runMetricsRecorder(ctx context.Context) {
	if !s.metricsOn || s.mrecorder == nil || s.mcollector == nil {
		return
	}
	_ = s.mrecorder.Prune(time.Now(), metricsRetentionDays)
	lastPruneDay := time.Now().Day()
	t := time.NewTicker(metricsRecordInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.recordOnce(ctx)
			if d := time.Now().Day(); d != lastPruneDay {
				_ = s.mrecorder.Prune(time.Now(), metricsRetentionDays)
				lastPruneDay = d
			}
		}
	}
}

// recordOnce samples and appends, recovering from any panic in collection.
func (s *Server) recordOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("daemon: metrics recorder recovered panic: %v", rec)
		}
	}()
	sample, err := s.mcollector.Sample(ctx)
	if err != nil {
		log.Printf("daemon: metrics sample failed: %v", err)
		return
	}
	if err := s.mrecorder.Record(sample); err != nil {
		log.Printf("daemon: metrics record failed: %v", err)
	}
}

// storeAgentLister adapts the session store to metrics.Lister, returning only
// live (non-terminal) agents — the ones with a tmux pane worth attributing.
type storeAgentLister struct{ st store.Store }

func (l storeAgentLister) LiveAgents(ctx context.Context) ([]metrics.Agent, error) {
	sessions, err := l.st.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []metrics.Agent
	for _, sess := range sessions {
		if !liveStatus(sess.Status) {
			continue
		}
		out = append(out, metrics.Agent{ID: sess.ID, TmuxSession: sess.TmuxSession, Status: string(sess.Status)})
	}
	return out, nil
}
```

- [ ] **Step 6: Start the recorder goroutine**

In `internal/daemon/server.go`, inside `ListenAndServe`, add after the `runPressureSampler` block (after line 48):

```go
	if s.life != nil {
		go s.runPressureSampler(runCtx)
	}
	go s.runMetricsRecorder(runCtx)
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestHandleMetrics -v`
Expected: PASS (3 tests).

- [ ] **Step 8: Build the whole daemon package + commit**

Run: `go build ./internal/daemon/ && go test ./internal/daemon/`
Expected: build ok; tests `ok`.

```bash
git add internal/daemon/metrics_routes.go internal/daemon/metrics_routes_test.go internal/daemon/api.go internal/daemon/server.go
git commit -m "feat(daemon): /metrics + /metrics/history routes, recorder goroutine, store→Lister adapter"
```

---

## Task 8: Wire the collector + recorder in the daemon command

**Files:**
- Modify: `internal/cli/daemon.go`

- [ ] **Step 1: Add the metrics wiring**

In `internal/cli/daemon.go`, after the `srv.SetSpawnGate(...)` line (line 71), add:

```go
		srv.SetSpawnGate(cfg.SpawnGateEnabled, cfg.SpawnGateMaxAgents)
		mcol := metrics.NewCollector(runner, daemon.NewAgentLister(st), srv.PressureName)
		mrec, err := metrics.NewRecorder(filepath.Join(cfg.DataDir, "metrics"))
		if err != nil {
			return err
		}
		srv.SetMetrics(mcol, mrec, cfg.MetricsEnabled)
```

Add the import to `daemon.go`'s import block:

```go
	"github.com/srjn45/warden/internal/metrics"
```

- [ ] **Step 2: Export the two daemon helpers the wiring needs**

In `internal/daemon/metrics_routes.go`, add a constructor for the lister (the struct is unexported) and export `PressureName`:

```go
// NewAgentLister adapts a store into a metrics.Lister of live agents.
func NewAgentLister(st store.Store) metrics.Lister { return storeAgentLister{st: st} }

// PressureName returns the cached pressure level name (for the metrics collector).
func (s *Server) PressureName() string { return s.pressureName() }
```

- [ ] **Step 3: Build the binary**

Run: `go build ./...`
Expected: success (no output).

- [ ] **Step 4: Commit**

```bash
git add internal/cli/daemon.go internal/daemon/metrics_routes.go
git commit -m "feat(daemon): wire metrics collector + recorder into the daemon command"
```

---

## Task 9: Client methods `GetMetrics` / `GetMetricsHistory`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/metrics_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Write([]byte(`{"taken_at":"2026-06-09T10:00:00Z","system":{"agent_count":3,"attributed_rss_bytes":1048576},"agents":[{"id":"a","rss_bytes":1024}],"daemon":{"goroutines":5}}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	m, err := c.GetMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.System.AgentCount != 3 || len(m.Agents) != 1 || m.Agents[0].ID != "a" {
		t.Fatalf("metrics=%+v", m)
	}
}

func TestGetMetricsHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"samples":[{"system":{"agent_count":2}}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	got, err := c.GetMetricsHistory(context.Background(), "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].System.AgentCount != 2 {
		t.Fatalf("history=%+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestGetMetrics -v`
Expected: FAIL — `c.GetMetrics` undefined.

- [ ] **Step 3: Implement (append to client.go)**

Add the import (the metrics package) to `client.go`'s import block:

```go
	"github.com/srjn45/warden/internal/metrics"
```

Then append the methods:

```go
// GetMetrics fetches the live resource snapshot (GET /metrics).
func (c *Client) GetMetrics(ctx context.Context) (*metrics.Sample, error) {
	var s metrics.Sample
	if err := c.do(ctx, http.MethodGet, "/metrics", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetMetricsHistory fetches recorded samples (GET /metrics/history). since is an
// RFC3339 timestamp ("" lets the daemon default to its look-back window); limit
// <= 0 lets the daemon pick its cap.
func (c *Client) GetMetricsHistory(ctx context.Context, since string, limit int) ([]metrics.Sample, error) {
	p := "/metrics/history"
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if e := q.Encode(); e != "" {
		p += "?" + e
	}
	var resp struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if err := c.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Samples, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run TestGetMetrics -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/metrics_test.go
git commit -m "feat(client): GetMetrics + GetMetricsHistory"
```

---

## Task 10: CLI `warden stats`

**Files:**
- Create: `internal/cli/stats.go`
- Test: `internal/cli/stats_test.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Write the failing test (pure formatter)**

```go
package cli

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/metrics"
)

func TestFormatStatsSortsByRSS(t *testing.T) {
	s := &metrics.Sample{
		System: metrics.SystemStats{TotalBytes: 16 << 30, UsedBytes: 8 << 30, PressureLevel: "normal", AgentCount: 2, AttributedRSSBytes: 3 << 30},
		Agents: []metrics.AgentStat{
			{ID: "small", Paneable: true, RSSBytes: 1 << 30, CPUPercent: 1, ProcCount: 2, UptimeSec: 60},
			{ID: "big", Paneable: true, RSSBytes: 2 << 30, CPUPercent: 9, ProcCount: 3, UptimeSec: 3600},
		},
	}
	out := formatStats(s)
	// "big" must appear before "small" (sorted by RSS desc).
	if strings.Index(out, "big") > strings.Index(out, "small") {
		t.Fatalf("expected big before small:\n%s", out)
	}
	if !strings.Contains(out, "pressure") || !strings.Contains(out, "agents") {
		t.Fatalf("summary line missing:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:          "0 B",
		1023:       "1023 B",
		1 << 10:    "1.0 KiB",
		1536:       "1.5 KiB",
		1 << 20:    "1.0 MiB",
		2 << 30:    "2.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d)=%q want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestFormatStats|TestHumanBytes' -v`
Expected: FAIL — `undefined: formatStats`.

- [ ] **Step 3: Implement `stats.go`**

```go
package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/metrics"
)

// humanBytes renders a byte count as a compact IEC string (B/KiB/MiB/GiB).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// humanDuration renders seconds as a compact "1h2m" / "3m" / "45s".
func humanDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// formatStats renders a Sample as a compact human report: a system summary line
// then per-agent rows sorted by RSS descending (the hog on top).
func formatStats(s *metrics.Sample) string {
	var b strings.Builder
	sys := s.System
	fmt.Fprintf(&b, "system: %s used / %s total · %s swap · pressure %s · %d agents · %s attributed\n",
		humanBytes(sys.UsedBytes), humanBytes(sys.TotalBytes), humanBytes(sys.SwapUsedBytes),
		sys.PressureLevel, sys.AgentCount, humanBytes(sys.AttributedRSSBytes))
	fmt.Fprintf(&b, "daemon: %s rss · %d goroutines · %d fds\n",
		humanBytes(s.Daemon.RSSBytes), s.Daemon.Goroutines, s.Daemon.OpenFDs)

	agents := append([]metrics.AgentStat(nil), s.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].RSSBytes > agents[j].RSSBytes })
	if len(agents) == 0 {
		b.WriteString("(no live agents)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-24s %10s %6s %5s %8s\n", "AGENT", "RSS", "CPU%", "PROCS", "UPTIME")
	for _, a := range agents {
		rss := humanBytes(a.RSSBytes)
		if !a.Paneable {
			rss = "—"
		}
		fmt.Fprintf(&b, "%-24s %10s %6.1f %5d %8s\n", a.ID, rss, a.CPUPercent, a.ProcCount, humanDuration(a.UptimeSec))
	}
	return b.String()
}

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			watch, _ := cmd.Flags().GetBool("watch")
			render := func() error {
				s, err := clientFor(cmd).GetMetrics(cmd.Context())
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(cmd.OutOrStdout(), s)
				}
				fmt.Fprint(cmd.OutOrStdout(), formatStats(s))
				return nil
			}
			if !watch {
				return render()
			}
			// --watch: clear + redraw on an interval until interrupted.
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				fmt.Fprint(cmd.OutOrStdout(), "\033[2J\033[H") // clear screen, home cursor
				if err := render(); err != nil {
					return err
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-t.C:
				}
			}
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	cmd.Flags().Bool("watch", false, "redraw every 3s until interrupted")
	return cmd
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/root.go`, add `newStatsCmd()` to an `AddCommand` call (alongside `newLsCmd(), newStatusCmd(), newDigestCmd()`):

```go
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd(), newStatsCmd())
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/cli/ -run 'TestFormatStats|TestHumanBytes' -v && go build ./...`
Expected: PASS (2 tests); build ok.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/stats.go internal/cli/stats_test.go internal/cli/root.go
git commit -m "feat(cli): warden stats (human table, --json, --watch)"
```

---

## Task 11: Web — types, API, pure history-shaping helper

**Files:**
- Create: `web/src/lib/metrics.ts`
- Test: `web/src/lib/metrics.test.ts`
- Modify: `web/src/lib/api.ts`

- [ ] **Step 1: Write the failing test**

```ts
import { describe, it, expect } from 'vitest';
import { historySeries, fmtBytes, type MetricsSample } from './metrics';

const sample = (t: string, rss: number, level: string): MetricsSample => ({
  taken_at: t,
  system: { total_bytes: 0, used_bytes: 0, free_bytes: 0, wired_bytes: 0, compressed_bytes: 0, swap_used_bytes: 0, pressure_level: level, agent_count: 0, attributed_rss_bytes: rss },
  agents: [],
  daemon: { rss_bytes: 0, goroutines: 0, open_fds: 0 },
});

describe('historySeries', () => {
  it('builds parallel x/rss/pressure arrays sorted oldest-first', () => {
    // input newest-first (as the daemon returns it)
    const data = [
      sample('2026-06-09T10:00:30Z', 300, 'critical'),
      sample('2026-06-09T10:00:00Z', 100, 'normal'),
    ];
    const s = historySeries(data);
    expect(s.t.length).toBe(2);
    expect(s.rssGiB[0]).toBeCloseTo(100 / 2 ** 30);
    expect(s.pressure[0]).toBe(1); // normal=1
    expect(s.pressure[1]).toBe(4); // critical=4
    expect(s.t[0]).toBeLessThan(s.t[1]); // oldest first
  });
});

describe('fmtBytes', () => {
  it('renders IEC units', () => {
    expect(fmtBytes(0)).toBe('0 B');
    expect(fmtBytes(1536)).toBe('1.5 KiB');
    expect(fmtBytes(2 * 2 ** 30)).toBe('2.0 GiB');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/lib/metrics.test.ts`
Expected: FAIL — cannot find module `./metrics`.

- [ ] **Step 3: Implement `metrics.ts`**

```ts
// Mirrors the daemon's metrics.Sample wire shape (internal/metrics/types.go).
export interface SystemStats {
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  wired_bytes: number;
  compressed_bytes: number;
  swap_used_bytes: number;
  pressure_level: string;
  agent_count: number;
  attributed_rss_bytes: number;
}

export interface AgentStat {
  id: string;
  status: string;
  paneable: boolean;
  rss_bytes: number;
  cpu_percent: number;
  proc_count: number;
  uptime_sec: number;
}

export interface DaemonStat {
  rss_bytes: number;
  goroutines: number;
  open_fds: number;
}

export interface MetricsSample {
  taken_at: string;
  system: SystemStats;
  agents: AgentStat[];
  daemon: DaemonStat;
}

// pressureRank maps a level name to its numeric severity (matches the Go enum:
// normal=1, warn=2, critical=4).
export function pressureRank(level: string): number {
  switch (level) {
    case 'warn': return 2;
    case 'critical': return 4;
    default: return 1;
  }
}

// HistorySeries is column-oriented data ready for a uPlot chart.
export interface HistorySeries {
  t: number[];        // unix seconds, oldest-first
  rssGiB: number[];   // attributed RSS in GiB
  pressure: number[]; // pressure rank 1/2/4
}

// historySeries converts daemon history (newest-first) into oldest-first
// parallel arrays for charting.
export function historySeries(samples: MetricsSample[]): HistorySeries {
  const ordered = [...samples].sort(
    (a, b) => new Date(a.taken_at).getTime() - new Date(b.taken_at).getTime(),
  );
  return {
    t: ordered.map((s) => new Date(s.taken_at).getTime() / 1000),
    rssGiB: ordered.map((s) => s.system.attributed_rss_bytes / 2 ** 30),
    pressure: ordered.map((s) => pressureRank(s.system.pressure_level)),
  };
}

// fmtBytes renders a byte count as a compact IEC string.
export function fmtBytes(b: number): string {
  if (b < 1024) return `${b} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let v = b / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}
```

- [ ] **Step 4: Add the API fetchers to `api.ts`**

Add the import at the top of `web/src/lib/api.ts` (extend the existing type import line):

```ts
import type { MetricsSample } from './metrics';
```

Append the fetchers (near `getPressure`):

```ts
export async function getMetrics(): Promise<MetricsSample> {
  return parse<MetricsSample>(await fetch('/metrics'));
}

export async function getMetricsHistory(sinceISO?: string, limit = 480): Promise<MetricsSample[]> {
  const q = new URLSearchParams();
  if (sinceISO) q.set('since', sinceISO);
  if (limit) q.set('limit', String(limit));
  const qs = q.toString();
  const data = await parse<{ samples: MetricsSample[] | null }>(
    await fetch(`/metrics/history${qs ? `?${qs}` : ''}`),
  );
  return data.samples ?? [];
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd web && npx vitest run src/lib/metrics.test.ts`
Expected: PASS (2 test groups).

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/metrics.ts web/src/lib/metrics.test.ts web/src/lib/api.ts
git commit -m "feat(web): metrics types, API fetchers, pure history-series helper"
```

---

## Task 12: Web — ResourcesPanel with uPlot history chart

**Files:**
- Modify: `web/package.json` (add uplot)
- Create: `web/src/components/ResourcesPanel.tsx`
- Modify: `web/src/components/OverviewTab.tsx`

- [ ] **Step 1: Add the uPlot dependency**

Run: `cd web && npm install uplot@^1.6.31`
Expected: `package.json` gains `"uplot"` under dependencies; `package-lock.json` updated.

- [ ] **Step 2: Implement `ResourcesPanel.tsx`**

```tsx
import { useEffect, useRef, useState } from 'react';
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { getMetrics, getMetricsHistory } from '../lib/api';
import { historySeries, fmtBytes, type MetricsSample } from '../lib/metrics';

// ResourcesPanel shows warden's live footprint (system memory + per-agent RSS/CPU)
// plus an attributed-RSS history chart, so a memory ramp is visible. Web-only.
export default function ResourcesPanel() {
  const [live, setLive] = useState<MetricsSample | null>(null);
  const [history, setHistory] = useState<MetricsSample[]>([]);
  const chartRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);

  // Poll the live snapshot + history every 5s.
  useEffect(() => {
    let alive = true;
    const tick = () => {
      getMetrics().then((m) => { if (alive) setLive(m); }).catch(() => {});
      getMetricsHistory().then((h) => { if (alive) setHistory(h); }).catch(() => {});
    };
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);

  // (Re)draw the uPlot chart whenever history changes.
  useEffect(() => {
    if (!chartRef.current) return;
    const s = historySeries(history);
    const data: uPlot.AlignedData = [s.t, s.rssGiB, s.pressure];
    if (!plotRef.current) {
      const opts: uPlot.Options = {
        width: chartRef.current.clientWidth || 600,
        height: 160,
        scales: { x: { time: true } },
        series: [
          {},
          { label: 'attributed RSS (GiB)', stroke: '#4ea1ff', width: 2 },
          { label: 'pressure', stroke: '#ff6b6b', width: 1, scale: 'p' },
        ],
        axes: [{}, { label: 'GiB' }, { scale: 'p', side: 1, label: 'pressure' }],
      };
      plotRef.current = new uPlot(opts, data, chartRef.current);
    } else {
      plotRef.current.setData(data);
    }
  }, [history]);

  // Tear down the plot on unmount.
  useEffect(() => () => { plotRef.current?.destroy(); plotRef.current = null; }, []);

  const agents = [...(live?.agents ?? [])].sort((a, b) => b.rss_bytes - a.rss_bytes);

  return (
    <div className="resources">
      {live && (
        <div className="resources-summary muted">
          {fmtBytes(live.system.used_bytes)} / {fmtBytes(live.system.total_bytes)} used ·{' '}
          {fmtBytes(live.system.swap_used_bytes)} swap · pressure {live.system.pressure_level} ·{' '}
          {fmtBytes(live.system.attributed_rss_bytes)} attributed across {live.system.agent_count} agents
        </div>
      )}
      <div ref={chartRef} className="resources-chart" />
      <table className="resources-table">
        <thead>
          <tr><th>agent</th><th>RSS</th><th>CPU%</th><th>procs</th></tr>
        </thead>
        <tbody>
          {agents.map((a) => (
            <tr key={a.id}>
              <td>{a.id}</td>
              <td>{a.paneable ? fmtBytes(a.rss_bytes) : '—'}</td>
              <td>{a.cpu_percent.toFixed(1)}</td>
              <td>{a.proc_count}</td>
            </tr>
          ))}
          {agents.length === 0 && <tr><td colSpan={4} className="muted">no live agents</td></tr>}
        </tbody>
      </table>
    </div>
  );
}
```

- [ ] **Step 3: Add the Resources section to the Overview tab**

In `web/src/components/OverviewTab.tsx`, add the import:

```tsx
import ResourcesPanel from './ResourcesPanel';
```

Add a section after the `Fleet` section (and before `Quick spawn`):

```tsx
      <section className="card">
        <h3>Resources</h3>
        <ResourcesPanel />
      </section>
```

- [ ] **Step 4: Verify the web build + tests pass**

Run: `cd web && npm run build && npx vitest run`
Expected: Astro build succeeds; all vitest suites pass (including `metrics.test.ts`).

- [ ] **Step 5: Commit**

```bash
git add web/package.json web/package-lock.json web/src/components/ResourcesPanel.tsx web/src/components/OverviewTab.tsx
git commit -m "feat(web): Resources panel with live table + uPlot RSS/pressure history"
```

---

## Task 13: Full verification + release build

**Files:** none (verification only)

- [ ] **Step 1: Run the whole Go suite**

Run: `go test ./...`
Expected: all packages `ok` (this is the authoritative gate). If the machine is under heavy load, the tmux/daemon packages may time out — re-run just those: `go test ./internal/metrics/ ./internal/daemon/ ./internal/cli/ ./internal/client/ ./internal/config/`.

- [ ] **Step 2: Vet + format**

Run: `gofmt -l internal/ && go vet ./...`
Expected: no files listed by gofmt; vet clean.

- [ ] **Step 3: Build the release binary (embeds web/dist)**

Run: `make release`
Expected: builds the web UI then the binary with no errors.

- [ ] **Step 4: Smoke-test the live endpoint against a throwaway daemon**

Run (in one shell): `WARDEN_DATA_DIR=/tmp/warden-smoke WARDEN_ADDR=127.0.0.1:8799 ./bin/warden daemon`
Run (in another): `curl -s 127.0.0.1:8799/metrics | head -c 400; echo; ./bin/warden --addr 127.0.0.1:8799 stats`
Expected: `/metrics` returns a JSON Sample; `warden stats` prints the system summary + (likely empty) agent table. Stop the daemon with Ctrl-C.

- [ ] **Step 5: Confirm the recorder wrote a day-file**

Run: `ls -la /tmp/warden-smoke/metrics/ && head -1 /tmp/warden-smoke/metrics/*.jsonl`
Expected: a `YYYY-MM-DD.jsonl` exists (dir perms `drwx------`), first line is a JSON Sample.

- [ ] **Step 6: Final commit (if any verification fixups were needed)**

```bash
git add -A
git commit -m "chore(observability): verification fixups" || echo "nothing to commit"
```

---

## Notes for the implementer

- **Manual browser smoke is left for the user** (consistent with prior warden features): after `make install` + daemon restart, open the web UI Overview tab and confirm the Resources panel renders the live table and the history chart populates over a minute or two. The plan's automated gates cover everything else.
- **Daemon restart required** to serve the new web bundle + enable the recorder: the running daemon predates this change, so `make install` (or the repo's reinstall script) + a daemon restart is needed for the live feature.
- **macOS-first:** `vm_stat`/`sysctl vm.swapusage`/BSD `ps` are macOS shapes. On Linux these commands differ; the collector degrades (zeros) rather than crashing. Cross-platform parsing is explicit future work.
- The `0o700` permission discipline lands here via `NewRecorder`; if you also want to tighten the existing session dirs in the same branch (the spec mentions it), that is a separate one-line change in `internal/store/file.go` — out of this plan's task list unless the user asks.
