package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Integration harness: the "installed binary" is a real executable (a copy of
// the test binary with a VERSION=…;MODE=… trailer) that, when launched with
// WARDEN_STUB_DAEMON=1, behaves as a stub daemon: serves /healthz, or crashes
// at startup writing its refusal to stderr.

const stubMarker = "\n--WARDEN-STUB--"

func TestMain(m *testing.M) {
	if os.Getenv("WARDEN_STUB_DAEMON") == "1" {
		runStubDaemon()
		return
	}
	os.Exit(m.Run())
}

func runStubDaemon() {
	exe, _ := os.Executable()
	raw, _ := os.ReadFile(exe)
	i := bytes.LastIndex(raw, []byte(stubMarker))
	version, mode := "0.0.0", "ok"
	if i >= 0 {
		for _, kv := range strings.Split(strings.TrimSpace(string(raw[i+len(stubMarker):])), ";") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "VERSION":
				version = v
			case "MODE":
				mode = v
			}
		}
	}
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "backend registry: refusing to open: id 1 update has revision 68 after revision 70")
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": version})
	})
	_ = http.ListenAndServe(os.Getenv("WARDEN_STUB_ADDR"), mux)
	os.Exit(1)
}

func stubBinary(t *testing.T, path, version, mode string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	raw, err := os.ReadFile(exe)
	require.NoError(t, err)
	raw = append(raw, []byte(fmt.Sprintf("%sVERSION=%s;MODE=%s", stubMarker, version, mode))...)
	require.NoError(t, os.WriteFile(path, raw, 0o755))
}

// procService supervises the installed binary as a child process.
type procService struct {
	bin, addr, errFile string
	mu                 sync.Mutex
	cmd                *exec.Cmd
	done               chan struct{}
}

func (p *procService) start() error {
	p.stop()
	f, err := os.Create(p.errFile)
	if err != nil {
		return err
	}
	cmd := exec.Command(p.bin)
	cmd.Env = append(os.Environ(), "WARDEN_STUB_DAEMON=1", "WARDEN_STUB_ADDR="+p.addr)
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); f.Close(); close(done) }()
	p.mu.Lock()
	p.cmd, p.done = cmd, done
	p.mu.Unlock()
	return nil
}

func (p *procService) stop() {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		<-done
	}
}

func (p *procService) State(context.Context) ServiceState {
	return ServiceState{Kind: ServiceSystemd, Active: true}
}
func (p *procService) Restart(context.Context) error { return p.start() }
func (p *procService) Exited(context.Context) bool {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
func (p *procService) Diagnostics(context.Context) string {
	b, _ := os.ReadFile(p.errFile)
	return string(b)
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().String()
}

func integrationEnv(t *testing.T, newMode, newAdvertised string) (*txn, *procService, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "warden")
	staged := filepath.Join(dir, "staged")
	stubBinary(t, bin, "1.0.0", "ok")
	stubBinary(t, staged, newAdvertised, newMode)
	addr := freeAddr(t)
	svc := &procService{bin: bin, addr: addr, errFile: filepath.Join(dir, "daemon.err")}
	require.NoError(t, svc.start())
	t.Cleanup(svc.stop)
	opts := Options{CurrentVersion: "1.0.0", InstallBin: bin, Stdout: &bytes.Buffer{}, ReadyTimeout: 15 * time.Second}
	tx := &txn{opts: opts, svc: svc, probe: httpProber{url: "http://" + addr + "/healthz"},
		inst: fsInstaller{bin: bin}, clock: realClock{}, target: "2.0.0"}
	// wait for the v1 stub to come up so capture sees a running daemon
	require.NoError(t, (&txn{opts: opts, svc: svc, probe: tx.probe, clock: realClock{}}).waitReady(context.Background(), "1.0.0", schemaAny))
	return tx, svc, staged
}

func TestIntegrationCleanUpdate(t *testing.T) {
	tx, _, staged := integrationEnv(t, "ok", "2.0.0")
	tx.capture(context.Background())
	require.True(t, tx.pre.DaemonRunning)
	require.Equal(t, "1.0.0", tx.pre.DaemonVersion)
	_, err := tx.apply(context.Background(), staged)
	require.NoError(t, err)
	h, err := tx.probe.Probe(context.Background())
	require.NoError(t, err)
	require.Equal(t, "2.0.0", h.Version)
}

func TestIntegrationDaemonCrashRollsBackWithDiagnosis(t *testing.T) {
	tx, _, staged := integrationEnv(t, "crash", "2.0.0")
	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), staged)
	require.Error(t, err)
	require.True(t, rb)
	require.Contains(t, err.Error(), "revision 68 after revision 70")
	require.Contains(t, err.Error(), "startup failure")
	h, perr := tx.probe.Probe(context.Background())
	require.NoError(t, perr)
	require.Equal(t, "1.0.0", h.Version, "prior version is serving again")
}

func TestIntegrationWrongVersionRollsBack(t *testing.T) {
	tx, _, staged := integrationEnv(t, "ok", "1.5.0")
	tx.opts.ReadyTimeout = 2 * time.Second
	tx.capture(context.Background())
	rb, err := tx.apply(context.Background(), staged)
	var wv *WrongVersionError
	require.ErrorAs(t, err, &wv)
	require.True(t, rb)
	h, perr := tx.probe.Probe(context.Background())
	require.NoError(t, perr)
	require.Equal(t, "1.0.0", h.Version)
}
