package cli

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ownerlock"
)

func TestMain(m *testing.M) {
	if d := os.Getenv("WARDEN_CLI_HOLD_DIR"); d != "" {
		if _, err := ownerlock.Acquire(d, ownerlock.Info{Kind: ownerlock.KindDaemon}); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("OWNER\n")
		select {}
	}
	// never probe the real data dir from CLI tests
	probeDataDirOwner = func(string) (*ownerlock.OwnedError, error) { return nil, nil }
	os.Exit(m.Run())
}

func TestCLIDirectOpenRefusedWhileDaemonOwns(t *testing.T) {
	data := t.TempDir()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "WARDEN_CLI_HOLD_DIR="+data)
	out, err := c.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	buf := make([]byte, 8)
	n, _ := out.Read(buf)
	require.Equal(t, "OWNER\n", string(buf[:n]))

	cliOwnership = nil
	err = ownCLIDataDir(data)
	require.ErrorIs(t, err, ownerlock.ErrOwned)
	require.Contains(t, err.Error(), "single writer")
	require.Nil(t, cliOwnership)
}

func TestCLIDirectOpenHoldsLockAgainstDaemon(t *testing.T) {
	data := t.TempDir()
	cliOwnership = nil
	require.NoError(t, ownCLIDataDir(data))
	require.NoError(t, ownCLIDataDir(data), "re-entrant within one command")
	_, err := ownerlock.Acquire(data, ownerlock.Info{Kind: ownerlock.KindDaemon})
	require.ErrorIs(t, err, ownerlock.ErrOwned)
	require.NoError(t, cliOwnership.Release())
	cliOwnership = nil
}
