package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "")
	t.Setenv("AGENTCTL_MONGO_URI", "")
	t.Setenv("AGENTCTL_DB", "")
	c := Load()
	require.Equal(t, "127.0.0.1:8765", c.Addr)
	require.Equal(t, "mongodb://localhost:27017", c.MongoURI)
	require.Equal(t, "agentctl", c.DB)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("AGENTCTL_ADDR", "127.0.0.1:9000")
	t.Setenv("AGENTCTL_MONGO_URI", "mongodb://db:27017")
	t.Setenv("AGENTCTL_DB", "test")
	c := Load()
	require.Equal(t, "127.0.0.1:9000", c.Addr)
	require.Equal(t, "mongodb://db:27017", c.MongoURI)
	require.Equal(t, "test", c.DB)
}
