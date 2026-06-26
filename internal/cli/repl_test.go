package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Without local_llm, interactive mode still starts: it notes the NL half is off
// and the deterministic /commands keep working (here /help, which needs neither
// a daemon nor a model).
func TestRepl_StartsWithoutLocalLLM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("local_llm: false\n"), 0o600))

	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetIn(strings.NewReader("/help\nexit\n"))
	root.SetArgs([]string{"repl", "--config", path})
	require.NoError(t, root.Execute())
	require.Contains(t, out.String(), "natural-language mode is off")
	require.Contains(t, out.String(), "/spawn", "deterministic commands are listed by /help")
}

func TestRepl_IsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"repl", "interactive", "i"} {
		c, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, "repl", c.Name(), "alias %q resolves to repl", name)
	}
}
