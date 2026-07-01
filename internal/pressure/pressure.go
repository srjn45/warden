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
//
// Elevated means the spawn is BLOCKED (428, requires force). Advisory means the
// spawn PROCEEDS but the caller should surface Reason — it is a non-blocking
// heads-up. The two are mutually exclusive: a blocking verdict is never also
// advisory.
type Verdict struct {
	Elevated   bool   `json:"elevated"`
	Advisory   bool   `json:"advisory"`
	Level      Level  `json:"level"`
	AgentCount int    `json:"agent_count"`
	MaxAgents  int    `json:"max_agents"`
	Reason     string `json:"reason"`
}

// Evaluate decides the gate verdict for a spawn.
//
// A spawn is BLOCKED (Elevated) only when the OS pressure is Critical (imminent
// swap) OR the live agent count has reached maxAgents. Warn is deliberately NOT
// blocking: it is a common, usually-recoverable macOS state, and hard-gating
// every spawn there just trained operators to --force reflexively. Warn instead
// yields an ADVISORY verdict — the spawn proceeds and the caller surfaces the
// reason. A maxAgents <= 0 disables the count co-trigger (pressure-only gating).
func Evaluate(level Level, agentCount, maxAgents int) Verdict {
	byCritical := level >= Critical
	byCount := maxAgents > 0 && agentCount >= maxAgents
	elevated := byCritical || byCount
	// Warn is advisory only, and is subsumed when the spawn already blocks.
	advisory := level == Warn && !elevated
	v := Verdict{
		Elevated:   elevated,
		Advisory:   advisory,
		Level:      level,
		AgentCount: agentCount,
		MaxAgents:  maxAgents,
	}
	switch {
	case byCritical && byCount:
		v.Reason = fmt.Sprintf("pressure: %s · %d agents live ≥ %d", level, agentCount, maxAgents)
	case byCritical:
		v.Reason = fmt.Sprintf("pressure: %s", level)
	case byCount:
		v.Reason = fmt.Sprintf("%d agents live ≥ %d", agentCount, maxAgents)
	case advisory:
		v.Reason = fmt.Sprintf("pressure: %s (advisory — spawning anyway)", level)
	}
	return v
}
