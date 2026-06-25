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
//
// ContextTokens, RuntimeSec, and FilesModified carry the non-resource history
// dimensions: the context-window fill (from the session record, no extra I/O),
// wall-clock runtime since the agent was created, and the count of uncommitted
// changed files in its worktree. They are best-effort and 0 when unavailable.
type AgentStat struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	Paneable      bool    `json:"paneable"`
	RSSBytes      uint64  `json:"rss_bytes"`
	CPUPercent    float64 `json:"cpu_percent"`
	ProcCount     int     `json:"proc_count"`
	UptimeSec     int64   `json:"uptime_sec"`
	ContextTokens int     `json:"context_tokens,omitempty"`
	RuntimeSec    int64   `json:"runtime_sec,omitempty"`
	FilesModified int     `json:"files_modified,omitempty"`
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
// ContextTokens/CreatedAt/Workdir feed the per-agent history dimensions
// (context fill, runtime, changed-file count); all are optional.
type Agent struct {
	ID            string
	TmuxSession   string
	Status        string
	ContextTokens int
	CreatedAt     time.Time
	Workdir       string
}
