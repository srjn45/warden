package config

import (
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Addr              string
	DataDir           string
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

// notifyEnabled reads AGENTCTL_NOTIFY; off by default, on only for 1/on/true.
// Notifications proved low-value, so they're opt-in for now.
func notifyEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_NOTIFY")) {
	case "1", "on", "true":
		return true
	}
	return false
}

// Load reads config from environment, applying defaults.
func Load() Config {
	return Config{
		Addr:              envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		DataDir:           envOr("AGENTCTL_DATA_DIR", defaultDataDir()),
		ClaudeProjectsDir: envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:     notifyEnabled(),
	}
}
