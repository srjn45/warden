package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Addr               string
	DataDir            string
	ClaudeProjectsDir  string
	NotifyEnabled      bool
	ApprovalsEnabled   bool
	AutoApproveEnabled bool // WARDEN_AUTO_APPROVE setting
	SpawnGateEnabled   bool
	SpawnGateMaxAgents int
	MetricsEnabled     bool
	AllowNonLoopback   bool
	TokenGuard         bool
	TokenWarnAlert     bool
	TokenAutoCompact   bool
	TokenWarn          int
	TokenCritical      int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// env reads a warden config variable, preferring the canonical WARDEN_<name>
// and falling back to the legacy AGENTCTL_<name> for back-compat.
func env(name string) string {
	if v := os.Getenv("WARDEN_" + name); v != "" {
		return v
	}
	return os.Getenv("AGENTCTL_" + name)
}

// envOr2 reads a warden config variable (WARDEN_<name> then AGENTCTL_<name>),
// returning def when neither is set.
func envOr2(name, def string) string {
	if v := env(name); v != "" {
		return v
	}
	return def
}

func defaultClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".warden"
	}
	return filepath.Join(home, ".warden")
}

// notifyEnabled reads WARDEN_NOTIFY (legacy AGENTCTL_NOTIFY); off by default, on
// only for 1/on/true. Notifications proved low-value, so they're opt-in for now.
func notifyEnabled() bool {
	switch strings.ToLower(env("NOTIFY")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// approvalsEnabled reads WARDEN_APPROVALS (legacy AGENTCTL_APPROVALS); ON by
// default, disabled only for 0/off/false. Gates the approvals-inbox feature
// (parse + inline answer).
func approvalsEnabled() bool {
	switch strings.ToLower(env("APPROVALS")) {
	case "0", "off", "false":
		return false
	}
	return true
}

// autoApproveEnabled reads WARDEN_AUTO_APPROVE (legacy AGENTCTL_AUTO_APPROVE);
// OFF by default (opt-in safety), enabled only for 1/on/true. Gates the
// auto-approval feature (automatic option 1 selection for recognized prompts).
func autoApproveEnabled() bool {
	switch strings.ToLower(env("AUTO_APPROVE")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// spawnGateEnabled reads WARDEN_SPAWN_GATE (legacy AGENTCTL_SPAWN_GATE); ON by
// default (the gate is soft, never hard-blocks), disabled only for 0/off/false.
func spawnGateEnabled() bool {
	switch strings.ToLower(env("SPAWN_GATE")) {
	case "0", "off", "false":
		return false
	}
	return true
}

// spawnGateMaxAgents reads WARDEN_SPAWN_GATE_MAX_AGENTS (legacy
// AGENTCTL_SPAWN_GATE_MAX_AGENTS, default 5). The count co-trigger fires when
// this many agents are already live. Unparseable → 5.
func spawnGateMaxAgents() int {
	if v := env("SPAWN_GATE_MAX_AGENTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 5
}

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

// allowNonLoopback reads WARDEN_ALLOW_NONLOOPBACK (legacy AGENTCTL_ALLOW_NONLOOPBACK);
// OFF by default, on only for 1/on/true. Gates binding the auth-less daemon to a
// non-loopback address.
func allowNonLoopback() bool {
	switch strings.ToLower(env("ALLOW_NONLOOPBACK")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// IsLoopbackHost reports whether addr (host:port, or a bare host) binds only the
// loopback interface. An empty host (e.g. ":8765") binds all interfaces and is
// NOT loopback. Unresolvable hostnames are treated as non-loopback (fail safe).
// No DNS lookups.
func IsLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port present
	}
	host = strings.TrimSpace(host)
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Load reads config from environment, applying defaults.
func Load() Config {
	tWarn := envInt("TOKEN_WARN", 200000)
	tCrit := envInt("TOKEN_CRITICAL", 400000)
	if tCrit <= tWarn { // inverted/degenerate config → defaults (warning must be reachable)
		tWarn, tCrit = 200000, 400000
	}
	return Config{
		Addr:               envOr2("ADDR", "127.0.0.1:8765"),
		DataDir:            envOr2("DATA_DIR", defaultDataDir()),
		ClaudeProjectsDir:  envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:      notifyEnabled(),
		ApprovalsEnabled:   approvalsEnabled(),
		AutoApproveEnabled: autoApproveEnabled(),
		SpawnGateEnabled:   spawnGateEnabled(),
		SpawnGateMaxAgents: spawnGateMaxAgents(),
		MetricsEnabled:     metricsEnabled(),
		AllowNonLoopback:   allowNonLoopback(),
		TokenGuard:         onByDefault("TOKEN_GUARD"),
		TokenWarnAlert:     onByDefault("TOKEN_WARN_ALERT"),
		TokenAutoCompact:   onByDefault("TOKEN_AUTO_COMPACT"),
		TokenWarn:          tWarn,
		TokenCritical:      tCrit,
	}
}
