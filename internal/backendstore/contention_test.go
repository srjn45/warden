package backendstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ownerlock"
)

// writerMain is a subprocess writer: it contends for the data-dir ownership
// lock (guarded=1) and, when it wins, opens the registry and hammers SetTier.
func init() {
	if os.Getenv("WARDEN_BS_WRITER") == "" {
		return
	}
	data := os.Getenv("WARDEN_BS_DATA")
	if os.Getenv("WARDEN_BS_WRITER") == "hold" {
		if _, err := ownerlock.Acquire(data, ownerlock.Info{Kind: ownerlock.KindDaemon}); err != nil {
			fmt.Println("REFUSED")
			os.Exit(3)
		}
		fmt.Println("OWNER")
		time.Sleep(time.Hour)
	}
	if os.Getenv("WARDEN_BS_GUARDED") == "1" {
		l, err := ownerlock.Acquire(data, ownerlock.Info{Kind: ownerlock.KindCLI})
		if err != nil {
			fmt.Println("REFUSED")
			os.Exit(0)
		}
		defer l.Release()
	}
	s, err := NewStore(filepath.Join(data, "backends"))
	if err != nil {
		fmt.Println("OPENFAIL", err)
		os.Exit(0)
	}
	for i := 0; i < 40; i++ {
		tier := []ModelTier{Tier1, Tier2, Tier3}[i%3]
		_ = s.SetRoleTier("implementer", tier)
	}
	_ = s.Close()
	fmt.Println("WROTE")
	os.Exit(0)
}

func runWriters(t *testing.T, data string, n int, guarded bool) []string {
	t.Helper()
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=NONE")
			g := "0"
			if guarded {
				g = "1"
			}
			c.Env = append(os.Environ(), "WARDEN_BS_WRITER=1", "WARDEN_BS_DATA="+data, "WARDEN_BS_GUARDED="+g)
			b, _ := c.CombinedOutput()
			out[i] = strings.TrimSpace(string(b))
		}()
	}
	wg.Wait()
	return out
}

// With the ownership lock, concurrent processes serialise (losers are refused),
// the registry stays verifiably clean — no revision regression can be produced
// by a second writer.
func TestContentionGuardedWritersNeverRegress(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "backends")
	s, err := NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, Reconcile(s, nil, time.Now()))
	require.NoError(t, s.Close())

	for round := 0; round < 3; round++ {
		res := runWriters(t, data, 6, true)
		wrote := 0
		for _, r := range res {
			require.NotContains(t, r, "OPENFAIL", r)
			if strings.Contains(r, "WROTE") {
				wrote++
			}
		}
		require.GreaterOrEqual(t, wrote, 1)
		rep, err := Verify(context.Background(), dir)
		require.NoError(t, err)
		require.True(t, rep.Clean(), "round %d: %+v", round, rep)
	}
}

// Repair refuses while a daemon owns the data dir, and works once released.
func TestRepairRefusedWhileDaemonOwns(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "backends")
	s, err := NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	release := holdAsForeign(t, data)
	_, err = Repair(context.Background(), dir, Options{BackupDir: t.TempDir()})
	require.ErrorIs(t, err, ErrOwned)
	require.ErrorIs(t, err, ownerlock.ErrOwned)
	require.Contains(t, err.Error(), "systemctl --user status warden")
	release()
	_, err = Repair(context.Background(), dir, Options{BackupDir: t.TempDir()})
	require.NoError(t, err)
}

// holdAsForeign starts a subprocess that owns the data dir like a daemon would.
func holdAsForeign(t *testing.T, data string) (release func()) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=NONE")
	c.Env = append(os.Environ(), "WARDEN_BS_WRITER=hold", "WARDEN_BS_DATA="+data)
	out, err := c.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, c.Start())
	buf := make([]byte, 16)
	n, _ := out.Read(buf)
	require.Contains(t, string(buf[:n]), "OWNER")
	done := false
	release = func() {
		if !done {
			done = true
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}
	t.Cleanup(release)
	return release
}
