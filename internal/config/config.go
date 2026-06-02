package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Addr              string
	MongoURI          string
	DB                string
	Workdir           string
	ClaudeProjectsDir string
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

func defaultWorkdir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "agentctl-agents"
	}
	return filepath.Join(home, "agentctl-agents")
}

// Load reads config from environment, applying defaults.
func Load() Config {
	return Config{
		Addr:              envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		MongoURI:          envOr("AGENTCTL_MONGO_URI", "mongodb://localhost:27017"),
		DB:                envOr("AGENTCTL_DB", "agentctl"),
		Workdir:           envOr("AGENTCTL_WORKDIR", defaultWorkdir()),
		ClaudeProjectsDir: envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
	}
}
