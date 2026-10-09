package ownerlock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// helper subprocess: the test binary re-executed. WARDEN_OL_HELPER=hold holds
// the lock until killed; =try prints the outcome of one Acquire.
func TestMain(m *testing.M) {
	switch os.Getenv("WARDEN_OL_HELPER") {
	case "hold":
		l, err := Acquire(os.Getenv("WARDEN_OL_DIR"), Info{Kind: KindDaemon, Version: "vtest", Addr: "127.0.0.1:1"})
		if err != nil {
			fmt.Println("REFUSED", err)
			os.Exit(3)
		}
		_ = l
		fmt.Println("OWNER")
		time.Sleep(time.Hour)
	case "try":
		_, err := Acquire(os.Getenv("WARDEN_OL_DIR"), Info{Kind: KindDaemon})
		if err != nil {
			fmt.Println("REFUSED", strings.ReplaceAll(err.Error(), "\n", " | "))
			os.Exit(3)
		}
		fmt.Println("OWNER")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(t *testing.T, mode, dir string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "WARDEN_OL_HELPER="+mode, "WARDEN_OL_DIR="+dir)
	return c
}

func startHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	c := helper(t, "hold", dir)
	out, err := c.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	buf := make([]byte, 16)
	n, _ := out.Read(buf)
	require.Contains(t, string(buf[:n]), "OWNER")
	return c
}

func TestSecondDaemonRefusedWithActionableError(t *testing.T) {
	dir := t.TempDir()
	h := startHolder(t, dir)

	start := time.Now()
	out, err := helper(t, "try", dir).CombinedOutput()
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "loser must exit promptly")
	s := string(out)
	require.Contains(t, s, "REFUSED")
	require.Contains(t, s, fmt.Sprintf("pid %d", h.Process.Pid))
	require.Contains(t, s, "systemctl --user status warden")
	require.Contains(t, s, "vtest")

	oe, perr := Probe(dir)
	require.NoError(t, perr)
	require.NotNil(t, oe)
	require.True(t, errors.Is(oe, ErrOwned))
	require.Equal(t, h.Process.Pid, oe.Owner.PID)
	require.Equal(t, "127.0.0.1:1", oe.Owner.Addr)
}

func TestRacingDaemonsExactlyOneOwner(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	res := make([]string, n)
	cmds := make([]*exec.Cmd, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := helper(t, "hold", dir)
			out, _ := c.StdoutPipe()
			if c.Start() != nil {
				return
			}
			cmds[i] = c
			buf := make([]byte, 64)
			k, _ := out.Read(buf)
			res[i] = string(buf[:k])
		}()
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, c := range cmds {
			if c != nil {
				_ = c.Process.Kill()
				_ = c.Wait()
			}
		}
	})
	owners := 0
	for _, r := range res {
		if strings.HasPrefix(r, "OWNER") {
			owners++
		} else {
			require.Contains(t, r, "REFUSED")
		}
	}
	require.Equal(t, 1, owners)
}

func TestCrashLeavesNoStuckLock(t *testing.T) {
	dir := t.TempDir()
	h := startHolder(t, dir)
	oe, _ := Probe(dir)
	require.NotNil(t, oe)

	require.NoError(t, h.Process.Signal(syscall.SIGKILL))
	_ = h.Wait()

	// The lock file (with stale metadata) is still on disk but not held.
	_, err := os.Stat(dir + "/" + FileName)
	require.NoError(t, err)
	oe, err = Probe(dir)
	require.NoError(t, err)
	require.Nil(t, oe)
	l, err := Acquire(dir, Info{Kind: KindDaemon})
	require.NoError(t, err)
	require.NoError(t, l.Release())
}

func TestSameProcessSecondAcquireRefusedProbeSelfNil(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, Info{Kind: KindCLI})
	require.NoError(t, err)
	defer l.Release()
	_, err = Acquire(dir, Info{Kind: KindDaemon})
	require.ErrorIs(t, err, ErrOwned)
	oe, err := Probe(dir)
	require.NoError(t, err)
	require.Nil(t, oe, "own lock is not foreign ownership")
}

func TestNextStepByLaunchMode(t *testing.T) {
	sd := (&OwnedError{Owner: &Owner{PID: 1, Kind: KindDaemon, Launch: LaunchSystemd}}).NextStep()
	require.Contains(t, sd, "systemctl --user stop warden")
	man := (&OwnedError{Owner: &Owner{PID: 7, Kind: KindDaemon, Launch: LaunchManual}}).NextStep()
	require.Contains(t, man, "manually launched")
	require.Contains(t, (&OwnedError{Owner: &Owner{PID: 7, Kind: KindCLI}}).NextStep(), "wait")
}
