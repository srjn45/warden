package config

import (
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
	SpawnGateEnabled   bool
	SpawnGateMaxAgents int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
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
		return ".agentctl"
	}
	return filepath.Join(home, ".agentctl")
}

// notifyEnabled reads AGENTCTL_NOTIFY; off by default, on only for 1/on/true.
// Notifications proved low-value, so they're opt-in for now.
func notifyEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_NOTIFY")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// approvalsEnabled reads AGENTCTL_APPROVALS; off by default, on only for
// 1/on/true. Gates the approvals-inbox feature (parse + inline answer).
func approvalsEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_APPROVALS")) {
	case "1", "on", "true":
		return true
	}
	return false
}

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

// Load reads config from environment, applying defaults.
func Load() Config {
	return Config{
		Addr:               envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		DataDir:            envOr("AGENTCTL_DATA_DIR", defaultDataDir()),
		ClaudeProjectsDir:  envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:      notifyEnabled(),
		ApprovalsEnabled:   approvalsEnabled(),
		SpawnGateEnabled:   spawnGateEnabled(),
		SpawnGateMaxAgents: spawnGateMaxAgents(),
	}
}
