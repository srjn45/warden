package config

import (
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Addr              string
	DataDir           string
	Workdir           string
	ClaudeProjectsDir string
	NotifyEnabled     bool
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

func defaultWorkdir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "agentctl-agents"
	}
	return filepath.Join(home, "agentctl-agents")
}

// notifyEnabled reads AGENTCTL_NOTIFY; on by default, off for 0/off/false.
func notifyEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_NOTIFY")) {
	case "0", "off", "false":
		return false
	}
	return true
}

// Load reads config from environment, applying defaults.
func Load() Config {
	return Config{
		Addr:              envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		DataDir:           envOr("AGENTCTL_DATA_DIR", defaultDataDir()),
		Workdir:           envOr("AGENTCTL_WORKDIR", defaultWorkdir()),
		ClaudeProjectsDir: envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:     notifyEnabled(),
	}
}
