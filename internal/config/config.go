package config

import "os"

type Config struct {
	Addr     string
	MongoURI string
	DB       string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads config from environment, applying defaults.
func Load() Config {
	return Config{
		Addr:     envOr("AGENTCTL_ADDR", "127.0.0.1:8765"),
		MongoURI: envOr("AGENTCTL_MONGO_URI", "mongodb://localhost:27017"),
		DB:       envOr("AGENTCTL_DB", "agentctl"),
	}
}
