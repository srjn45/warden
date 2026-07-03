// Package pressure models OS memory-pressure levels and the soft-spawn-gate
// verdict. It is pure: no exec, no I/O — parsing and the decision live here so
// they are unit-testable (mirrors internal/approval and internal/digest).
//
// Two kernel sources feed the same Level scale: macOS's
// kern.memorystatus_vm_pressure_level sysctl (ParseSysctl) and Linux's PSI
// memory file /proc/pressure/memory (ParsePSI).
package pressure

import (
	"fmt"
	"strconv"
	"strings"
)

// Level mirrors macOS kern.memorystatus_vm_pressure_level (1=normal, 2=warn,
// 4=critical). Linux PSI readings are mapped onto the same scale. The integer
// values cross the wire as-is.
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

// Linux PSI avg10 thresholds mapping /proc/pressure/memory onto Level.
//
// PSI "some" is the share of the last 10s in which at least one task stalled
// on memory; "full" is the share in which ALL non-idle tasks stalled (the
// system was doing nothing but reclaim — thrashing). Warn is advisory-only in
// the spawn gate, so its thresholds are moderate; Critical blocks spawns, so
// its thresholds are deliberately extreme (the frictionless-safeguards rule:
// a guard fires only when the machine is genuinely in trouble).
const (
	psiWarnSome     = 25.0 // some avg10 ≥ 25%: reclaim is routinely stalling tasks
	psiWarnFull     = 5.0  // full avg10 ≥ 5%: whole-system stalls have started
	psiCriticalSome = 60.0 // some avg10 ≥ 60%: most of the window spent stalled
	psiCriticalFull = 20.0 // full avg10 ≥ 20%: sustained thrash — imminent OOM territory
)

// ParsePSI parses the content of Linux's /proc/pressure/memory:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// and maps the avg10 readings onto Level via the psi* thresholds. Returns an
// error for input carrying no recognizable avg10 field so the caller can
// degrade to Normal (mirrors ParseSysctl). A kernel built with PSI disabled
// (psi=0) errors on read before parsing is ever reached.
func ParsePSI(raw string) (Level, error) {
	var someAvg10, fullAvg10 float64
	found := false
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		for _, f := range fields[1:] {
			v, ok := strings.CutPrefix(f, "avg10=")
			if !ok {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return 0, fmt.Errorf("pressure: unparseable PSI avg10 %q", f)
			}
			if kind == "some" {
				someAvg10 = n
			} else {
				fullAvg10 = n
			}
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("pressure: no PSI avg10 field in %q", strings.TrimSpace(raw))
	}
	switch {
	case fullAvg10 >= psiCriticalFull || someAvg10 >= psiCriticalSome:
		return Critical, nil
	case fullAvg10 >= psiWarnFull || someAvg10 >= psiWarnSome:
		return Warn, nil
	default:
		return Normal, nil
	}
}
