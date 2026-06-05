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
