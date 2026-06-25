package metrics

import (
	"context"
	"fmt"
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
		st := AgentStat{ID: ag.ID, Status: ag.Status, ContextTokens: ag.ContextTokens}
		pids := c.panePIDs(ctx, ag.TmuxSession)
		if len(pids) > 0 {
			rss, cpu, procs, uptime := aggregateTree(tbl, pids)
			st.Paneable = procs > 0
			st.RSSBytes, st.CPUPercent, st.ProcCount, st.UptimeSec = rss, cpu, procs, uptime
			attributed += rss
		}
		if !ag.CreatedAt.IsZero() {
			st.RuntimeSec = int64(out.TakenAt.Sub(ag.CreatedAt).Seconds())
		}
		st.FilesModified = c.changedFiles(ctx, ag.Workdir)
		out.Agents = append(out.Agents, st)
	}

	out.System = c.systemStats(ctx)
	out.System.AgentCount = len(agents)
	out.System.AttributedRSSBytes = attributed
	out.Daemon = c.daemonStats(ctx, tbl)
	return out, nil
}

// changedFiles counts uncommitted changes in the agent's worktree via one cheap
// `git status --porcelain`. Best-effort: 0 for an empty workdir or any git
// failure (not a repo, git absent). Each non-empty porcelain line is one path.
func (c *Collector) changedFiles(ctx context.Context, workdir string) int {
	if workdir == "" {
		return 0
	}
	out, err := c.Run(ctx, workdir, "git", "status", "--porcelain")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
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

func (c *Collector) daemonStats(ctx context.Context, tbl map[int]ProcRow) DaemonStat {
	pid := c.SelfPID
	if pid == 0 {
		pid = os.Getpid()
	}
	d := DaemonStat{Goroutines: runtime.NumGoroutine(), OpenFDs: c.openFDCount(ctx, pid)}
	if r, ok := tbl[pid]; ok {
		d.RSSBytes = r.RSSKiB * 1024
	}
	return d
}

// openFDCount counts the process's open file descriptors. On Linux it reads
// /proc/<pid>/fd (cheap, authoritative). Elsewhere (macOS/BSD have no /proc, and
// /dev/fd does not enumerate reliably under launchd) it falls back to counting
// the numbered fds in `lsof -F f`. Best-effort: 0 when neither is available.
func (c *Collector) openFDCount(ctx context.Context, pid int) int {
	if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		return len(entries)
	}
	out, err := c.Run(ctx, "", "lsof", "-wnP", "-p", strconv.Itoa(pid), "-F", "f")
	if err != nil {
		return 0
	}
	return parseLsofFDCount(out)
}
