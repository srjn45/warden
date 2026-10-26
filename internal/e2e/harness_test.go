// Package e2e holds process-level adversarial regressions for the agent-store
// integrity contract (#795): real `warden` binaries (daemon, doctor, repair) run
// as separate processes against seeded and fault-injected data directories.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/wardendb"
)

var wardenBin string

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "warden-e2e-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	wardenBin = filepath.Join(dir, "warden")
	args := []string{"build"}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "-o", wardenBin, "github.com/srjn45/warden/cmd/warden")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build warden: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is one isolated warden installation: its own HOME, tmux socket dir,
// config file and data dir. Nothing touches the developer's real warden/tmux.
type env struct {
	t       *testing.T
	root    string
	home    string
	data    string
	tmuxDir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	e := &env{t: t, root: root, home: filepath.Join(root, "home"), data: filepath.Join(root, "data"), tmuxDir: shortTmp(t)}
	require.NoError(t, os.MkdirAll(e.home, 0o700))
	require.NoError(t, os.MkdirAll(e.data, 0o700))
	t.Cleanup(func() { _ = e.tmux("kill-server") })
	return e
}

// shortTmp returns a short dir (tmux socket paths are length-limited).
func shortTmp(t *testing.T) string {
	d, err := os.MkdirTemp("", "wde2e")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// tmux runs tmux against this env's private server (TMUX_TMPDIR).
func (e *env) tmux(args ...string) error {
	c := exec.Command("tmux", args...)
	c.Env = e.environ()
	return c.Run()
}

func (e *env) tmuxPanePIDs() string {
	c := exec.Command("tmux", "list-panes", "-a", "-F", "#{session_name}:#{pane_pid}")
	c.Env = e.environ()
	out, _ := c.Output()
	return strings.TrimSpace(string(out))
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().String()
}

// config writes a config file whose data_dir is dataDir (any alias spelling).
func (e *env) config(addr, dataDir string) string {
	p := filepath.Join(e.root, fmt.Sprintf("cfg-%x.yaml", sha256.Sum256([]byte(addr+dataDir))))
	body := fmt.Sprintf("addr: %q\ndata_dir: %q\n", addr, dataDir)
	require.NoError(e.t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WARDEN_") || strings.HasPrefix(kv, "TMUX") || strings.HasPrefix(kv, "HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.home, "TMUX_TMPDIR="+e.tmuxDir, "WARDEN_NO_TUTORIAL=1")
}

func (e *env) cmd(ctx context.Context, cwd string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, wardenBin, args...)
	c.Dir = cwd
	c.Env = e.environ()
	return c
}

// run executes a short-lived warden command and returns combined output + exit.
func (e *env) run(cwd string, args ...string) (string, int) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := e.cmd(ctx, cwd, args...).CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		require.True(e.t, ok, "run %v: %v", args, err)
		code = ee.ExitCode()
	}
	return string(out), code
}

// daemon is a running `warden daemon` process.
type daemon struct {
	e    *env
	cmd  *exec.Cmd
	addr string
	log  *bytes.Buffer
	done chan struct{}
	err  error
}

func (e *env) startDaemon(addr, dataDir, cwd string) *daemon {
	e.t.Helper()
	cfg := e.config(addr, dataDir)
	d := &daemon{e: e, addr: addr, log: &bytes.Buffer{}, done: make(chan struct{})}
	d.cmd = e.cmd(context.Background(), cwd, "--config", cfg, "daemon")
	d.cmd.Stdout, d.cmd.Stderr = d.log, d.log
	require.NoError(e.t, d.cmd.Start())
	go func() { d.err = d.cmd.Wait(); close(d.done) }()
	e.t.Cleanup(func() { d.kill() })
	return d
}

func (d *daemon) exited() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

func (d *daemon) kill() {
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	<-d.done
}

func (d *daemon) stop() {
	d.e.t.Helper()
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		d.kill()
		d.e.t.Fatalf("daemon did not stop on SIGTERM\n%s", d.log.String())
	}
}

func (d *daemon) base() string { return "http://" + d.addr }

// waitUp blocks until /healthz answers, failing if the process exits first.
func (d *daemon) waitUp() {
	d.e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if d.exited() {
			d.e.t.Fatalf("daemon exited early: %v\n%s", d.err, d.log.String())
		}
		if r, err := http.Get(d.base() + "/healthz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.e.t.Fatalf("daemon never became healthy\n%s", d.log.String())
}

// waitExit blocks until the daemon process exits on its own and returns its log.
func (d *daemon) waitExit() string {
	d.e.t.Helper()
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		d.e.t.Fatalf("daemon did not exit\n%s", d.log.String())
	}
	return d.log.String()
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	r, err := c.Get(url)
	require.NoError(t, err)
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, b
}

// firstSSE returns the first data frame (and any named event) of the stream.
func firstSSE(t *testing.T, url string) (status int, frame string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		// No response headers before the deadline: the stream emitted nothing.
		return 0, ""
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	buf := make([]byte, 1<<16)
	var sb strings.Builder
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if strings.Contains(sb.String(), "\n\n") || err != nil {
			return r.StatusCode, sb.String()
		}
	}
}

type health struct {
	Healthy         bool `json:"healthy"`
	Degraded        bool `json:"degraded"`
	FailureCount    int  `json:"failure_count"`
	RepairAvailable bool `json:"repair_available"`
	NextStep        string
	Failures        []struct {
		Collection string `json:"collection"`
		Key        string `json:"key"`
		Class      string `json:"class"`
		Detail     string `json:"detail"`
	} `json:"failures"`
}

func (d *daemon) health() health {
	d.e.t.Helper()
	code, b := get(d.e.t, d.base()+"/api/v1/store/health")
	require.Equal(d.e.t, 200, code, string(b))
	var h health
	require.NoError(d.e.t, json.Unmarshal(b, &h), string(b))
	return h
}

// openShared opens the schema-3 warden-db in THIS process (stamping the ledger
// at schema 3 first, as a booted daemon would), so a test can seed it before the
// daemon under test becomes the only owner. The returned closer releases it.
func (e *env) openShared() (*agentstore.Store, func()) {
	e.t.Helper()
	if _, err := schema.Load(e.data); err != nil {
		require.NoError(e.t, schema.Save(e.data, &schema.Ledger{SchemaVersion: schema.SchemaVersion, BinaryVersion: "e2e",
			History: []schema.HistoryEntry{{From: 0, To: schema.SchemaVersion, Migration: schema.MigrationBaselineFresh}}}))
	}
	db, err := wardendb.Open(e.data)
	require.NoError(e.t, err)
	s, err := agentstore.NewSchema3StoreFromDB(db)
	require.NoError(e.t, err)
	return s, func() { require.NoError(e.t, db.Close()) }
}

// openSharedProjects is openShared for the project store.
func (e *env) openSharedProjects() (*projectstore.Store, func()) {
	e.t.Helper()
	_, closeAgents := e.openShared() // stamps the ledger
	closeAgents()
	db, err := wardendb.Open(e.data)
	require.NoError(e.t, err)
	ps, err := projectstore.NewSchema3StoreFromDB(db)
	require.NoError(e.t, err)
	return ps, func() { require.NoError(e.t, db.Close()) }
}

// seed opens the store in THIS process, inserts the agents, and closes it, so
// the daemon under test is the only owner afterwards.
func (e *env) seed(ids ...string) {
	e.t.Helper()
	s, closeDB := e.openShared()
	for _, id := range ids {
		require.NoError(e.t, s.Insert(context.Background(), &agentstore.Agent{ID: id, Name: "n-" + id, Scope: "system", Status: store.StatusWorking,
			TmuxSession: "tm-" + id, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	}
	closeDB()
}

// snapshot hashes every file under dir (path -> sha256) so tests can prove the
// live store was not mutated by a refusing/degraded/diagnostic process.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = hex.EncodeToString(h[:])
		return nil
	}))
	return out
}

func requireSame(t *testing.T, before, after map[string]string, msg string) {
	t.Helper()
	if len(before) != len(after) {
		var keys []string
		for k := range before {
			if _, ok := after[k]; !ok {
				keys = append(keys, "-"+k)
			}
		}
		for k := range after {
			if _, ok := before[k]; !ok {
				keys = append(keys, "+"+k)
			}
		}
		sort.Strings(keys)
		t.Fatalf("%s: file set changed: %v", msg, keys)
	}
	for k, v := range before {
		require.Equal(t, v, after[k], "%s: %s mutated", msg, k)
	}
}

// agentsDB restricts a snapshot to the agent collections of warden-db (the
// daemon registers every other collection on first open, which is not a
// mutation of the agent store under test).
func agentsDB(t *testing.T, e *env) map[string]string {
	out := map[string]string{}
	for _, col := range []string{"agents", "archived_agents"} {
		dir := filepath.Join(e.data, wardendb.DirName, col)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		for k, v := range snapshot(t, dir) {
			out[col+"/"+k] = v
		}
	}
	return out
}

func httpGetQuiet(url string) (int, error) {
	c := &http.Client{Timeout: 200 * time.Millisecond}
	r, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	r.Body.Close()
	return r.StatusCode, nil
}

// segments restricts agentsDB to the append-only record segments: the truth.
// Indexes and secondary indexes are derived state the engine may rebuild on open.
func segments(t *testing.T, e *env) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, v := range agentsDB(t, e) {
		if strings.Contains(filepath.Base(k), "seg_") {
			out[k] = v
		}
	}
	require.NotEmpty(t, out)
	return out
}

// fleet returns the tmux sessions of the agents the daemon lists (sorted), or
// the HTTP status when the listing is refused.
func fleet(t *testing.T, d *daemon) (int, []string) {
	t.Helper()
	code, body := get(t, d.base()+"/api/v1/sessions")
	var out struct {
		Sessions []struct {
			TmuxSession string `json:"tmux_session"`
		} `json:"sessions"`
	}
	if code == 200 {
		require.NoError(t, json.Unmarshal(body, &out), string(body))
	}
	var ids []string
	for _, s := range out.Sessions {
		ids = append(ids, s.TmuxSession)
	}
	sort.Strings(ids)
	return code, ids
}
