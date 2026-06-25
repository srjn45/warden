package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrch_RequiresLocalLLM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// default config has local_llm: false
	require.NoError(t, os.WriteFile(path, []byte("local_llm: false\n"), 0o600))

	root := newRootCmd()
	root.SetArgs([]string{"orch", "--config", path})
	err := root.Execute()
	require.ErrorContains(t, err, "local_llm")
}

func TestOrch_IsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"orch", "orchestrator"} {
		c, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, "orch", c.Name(), "alias %q resolves to orch", name)
	}
}
