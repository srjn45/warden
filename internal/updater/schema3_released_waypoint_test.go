package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Released-waypoint updater acceptance.
//
// The existing binary acceptance test drives the CURRENT updater package against
// a released daemon binary. That cannot catch a target that the already-shipped
// v12.1.0 updater (commit 5fceb280) mishandles, because the shipped updater is
// what actually runs on a user's machine when they type `wd update`.
//
// This test archives the released source, injects a harness into the archived
// internal/cli package that calls the UNCHANGED released updater.Apply (and
// updater.Rollback) with the released cli's own production Schema3 hooks
// (schema3Hooks: real `version --json` target info, real
// `migrate --verify-ownership` readiness), the released store preflights and
// the released TargetSchema/CurrentSchema wiring. Only the supervisor is
// substituted: a process ServiceController that runs the real installed
// waypoint / target daemon binaries as children on loopback with an isolated
// data dir, HOME and config. The release is served over local HTTP as a real
// tar.gz + checksums.txt + manifest.json built from the current checkout.
//
// Scope: full released Apply (download, verify, manifest gate, paired backup,
// swap, promote, handoff, restart, readiness, released finish()) and full
// released Rollback. Nothing in the finalizer path is replaced by current code
// or by a fake prober.
//
// Run through WARDEN_SCHEMA3_BINARY_ACCEPTANCE=1 (expensive isolated builds).

const releasedWaypointCommit = "5fceb280"

// The capability set the released updater (not the current one) demands of a
// schema-3 target; hardcoded so drift in the current constants is detected.
var releasedWaypointCapabilities = map[string]int{
	"stopped-daemon":              1,
	"schema3-transaction-handoff": 1,
	"verified-paired-backup":      1,
	"generation-restore":          1,
}

type releasedWaypointSpec struct {
	Scenario      string `json:"scenario"`
	DataDir       string `json:"data_dir"`
	ConfigPath    string `json:"config_path"`
	Addr          string `json:"addr"`
	InstallBin    string `json:"install_bin"`
	StagingDir    string `json:"staging_dir"`
	AssetBase     string `json:"asset_base"`
	Home          string `json:"home"`
	TargetVersion string `json:"target_version"`
	WaypointBin   string `json:"waypoint_bin"`
	TargetBin     string `json:"target_bin"`
}

func TestSchema3ReleasedWaypointUpdaterAcceptance(t *testing.T) {
	if os.Getenv("WARDEN_SCHEMA3_BINARY_ACCEPTANCE") != "1" {
		t.Skip("run with WARDEN_SCHEMA3_BINARY_ACCEPTANCE=1 for isolated released-updater builds")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("release tooling distributes Linux and Darwin binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	run := func(dir string, env []string, name string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		if env != nil {
			cmd.Env = append(os.Environ(), env...)
		}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s %v:\n%s", name, args, out)
		return out
	}

	// Archive the released source and give it the empty UI embed it expects.
	src := t.TempDir()
	tarball := filepath.Join(t.TempDir(), "waypoint.tar")
	run(root, nil, "git", "archive", "--format=tar", "--output="+tarball, releasedWaypointCommit)
	run(src, nil, "tar", "-xf", tarball, "-C", src)
	require.NoError(t, os.MkdirAll(filepath.Join(src, "web", "dist"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "web", "dist", ".gitkeep"), nil, 0o600))

	build := func(dir, version string) string {
		bin := filepath.Join(t.TempDir(), "warden")
		run(dir, nil, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/srjn45/warden/internal/cli.version="+version, "-o", bin, "./cmd/warden")
		return bin
	}
	waypointBin := build(src, "12.1.0")
	const targetVersion = "13.0.0"
	targetBin := build(root, targetVersion)

	// Inject the harness into the archived released cli package and compile it
	// once; each scenario then runs the compiled released-code test binary.
	require.NoError(t, os.WriteFile(filepath.Join(src, "internal", "cli", "zz_released_waypoint_harness_test.go"), []byte(releasedWaypointHarness), 0o600))
	harness := filepath.Join(t.TempDir(), "released-cli.test")
	run(src, nil, "go", "test", "-c", "-buildvcs=false", "-o", harness, "./internal/cli")

	// Release fixtures built from the real target binary.
	release := t.TempDir()
	archiveName := fmt.Sprintf("warden_%s_%s_%s.tar.gz", targetVersion, runtime.GOOS, runtime.GOARCH)
	writeReleasedWaypointArchive(t, filepath.Join(release, archiveName), targetBin)
	sum := releasedWaypointSHA256(t, filepath.Join(release, archiveName))
	require.NoError(t, os.WriteFile(filepath.Join(release, "checksums.txt"), []byte(sum+"  "+archiveName+"\n"), 0o600))
	manifest, err := json.Marshal(map[string]any{
		"version": targetVersion, "schema_version": 3, "min_schema": 2, "min_upgrade_from": "12.1.0",
		"waypoint": false, "breaking": true,
		"notes":    []string{"schema 3 ownership normalization", "paired-data rollback"},
		"requires": map[string]any{"capabilities": releasedWaypointCapabilities},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(release, "manifest.json"), manifest, 0o600))
	srv := httptest.NewServer(http.FileServer(http.Dir(release)))
	t.Cleanup(srv.Close)

	for _, scenario := range []string{"apply", "apply-then-rollback", "readiness-failure-rolls-back"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			data := filepath.Join(base, "data")
			require.NoError(t, os.MkdirAll(data, 0o700))
			home := filepath.Join(base, "home")
			require.NoError(t, os.MkdirAll(home, 0o700))
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			addr := l.Addr().String()
			require.NoError(t, l.Close())
			cfg := filepath.Join(data, "config.yaml")
			require.NoError(t, os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir: %s\naddr: %s\n", data, addr)), 0o600))
			installBin := filepath.Join(base, "bin", "warden")
			require.NoError(t, os.MkdirAll(filepath.Dir(installBin), 0o755))
			body, err := os.ReadFile(waypointBin)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(installBin, body, 0o755))
			specJSON, err := json.Marshal(releasedWaypointSpec{
				Scenario: scenario, DataDir: data, ConfigPath: cfg, Addr: addr, InstallBin: installBin,
				StagingDir: filepath.Join(base, "staging"), AssetBase: srv.URL, Home: home,
				TargetVersion: targetVersion, WaypointBin: waypointBin, TargetBin: targetBin,
			})
			require.NoError(t, err)
			specPath := filepath.Join(base, "spec.json")
			require.NoError(t, os.WriteFile(specPath, specJSON, 0o600))
			out := run(base, []string{"WD_RELEASED_WAYPOINT_SPEC=" + specPath}, harness,
				"-test.run", "^TestReleasedWaypointHarness$", "-test.v", "-test.count=1", "-test.timeout=8m")
			require.Contains(t, string(out), "PASS", "%s", out)
			require.NotContains(t, string(out), "--- SKIP")
		})
	}
}

func writeReleasedWaypointArchive(t *testing.T, path, bin string) {
	t.Helper()
	body, err := os.ReadFile(bin)
	require.NoError(t, err)
	f, err := os.Create(path)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "warden", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err = tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
}

func releasedWaypointSHA256(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	s := sha256.Sum256(body)
	return hex.EncodeToString(s[:])
}

// releasedWaypointHarness is compiled INTO the archived released internal/cli
// package, so it uses the released unexported production wiring.
const releasedWaypointHarness = `package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/updater"
)

type rwSpec struct {
	Scenario      string ` + "`json:\"scenario\"`" + `
	DataDir       string ` + "`json:\"data_dir\"`" + `
	ConfigPath    string ` + "`json:\"config_path\"`" + `
	Addr          string ` + "`json:\"addr\"`" + `
	InstallBin    string ` + "`json:\"install_bin\"`" + `
	StagingDir    string ` + "`json:\"staging_dir\"`" + `
	AssetBase     string ` + "`json:\"asset_base\"`" + `
	Home          string ` + "`json:\"home\"`" + `
	TargetVersion string ` + "`json:\"target_version\"`" + `
	WaypointBin   string ` + "`json:\"waypoint_bin\"`" + `
	TargetBin     string ` + "`json:\"target_bin\"`" + `
}

type rwLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *rwLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *rwLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// rwService supervises the installed binary as a real child daemon process.
type rwService struct {
	bin, cfg string
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	log      rwLog
	pids     []int
}

func (s *rwService) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *rwService) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := exec.Command(s.bin, "--config", s.cfg, "daemon")
	cmd.Stdout, cmd.Stderr = &s.log, &s.log
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	s.cmd, s.done = cmd, done
	s.pids = append(s.pids, cmd.Process.Pid)
	return nil
}

func (s *rwService) State(context.Context) updater.ServiceState {
	return updater.ServiceState{Kind: updater.ServiceSystemd, Active: s.running(), Detail: "rw-process-controller"}
}

func (s *rwService) Stop(context.Context) error {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

func (s *rwService) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	return s.start()
}

func (s *rwService) Exited(context.Context) bool {
	s.mu.Lock()
	has := s.cmd != nil
	s.mu.Unlock()
	return has && !s.running()
}

func (s *rwService) Diagnostics(context.Context) string { return s.log.String() }

func rwHealth(addr string) (status, version string, schemaVersion int, err error) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	var body struct {
		Status        string ` + "`json:\"status\"`" + `
		Version       string ` + "`json:\"version\"`" + `
		SchemaVersion int    ` + "`json:\"schema_version\"`" + `
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", 0, err
	}
	return body.Status, strings.TrimPrefix(body.Version, "v"), body.SchemaVersion, nil
}

// rwMutate issues a harmless mutating request (an invalid spawn body, so nothing
// is created even if it is let through).
func rwMutate(addr string) (int, string, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Post("http://"+addr+"/api/v1/agents", "application/json", strings.NewReader("{}"))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(b), nil
}

func rwWaitHealthy(t *testing.T, svc *rwService, addr, version string, schemaVersion int) {
	t.Helper()
	require.Eventually(t, func() bool {
		st, v, sv, err := rwHealth(addr)
		return err == nil && st == "ok" && v == version && sv == schemaVersion
	}, 40*time.Second, 200*time.Millisecond, "daemon never reported v%s schema %d; log:\n%s", version, schemaVersion, svc.log.String())
}

func TestReleasedWaypointHarness(t *testing.T) {
	specPath := os.Getenv("WD_RELEASED_WAYPOINT_SPEC")
	if specPath == "" {
		t.Skip("driven by the current checkout's released-waypoint acceptance test")
	}
	raw, err := os.ReadFile(specPath)
	require.NoError(t, err)
	var sp rwSpec
	require.NoError(t, json.Unmarshal(raw, &sp))

	// Isolate: no live HOME, no live warden credentials/config.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WARDEN_") {
			_ = os.Unsetenv(strings.SplitN(kv, "=", 2)[0])
		}
	}
	require.NoError(t, os.Setenv("HOME", sp.Home))
	require.NoError(t, os.Setenv("XDG_CONFIG_HOME", sp.Home+"/.config"))
	version = "12.1.0" // the released updater runs as the installed waypoint

	svc := &rwService{bin: sp.InstallBin, cfg: sp.ConfigPath}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	require.NoError(t, svc.start())
	rwWaitHealthy(t, svc, sp.Addr, "12.1.0", 2)
	originalPID := svc.pids[0]

	installedBefore, err := os.ReadFile(sp.InstallBin)
	require.NoError(t, err)
	waypointBytes, err := os.ReadFile(sp.WaypointBin)
	require.NoError(t, err)
	require.Equal(t, waypointBytes, installedBefore)
	targetBytes, err := os.ReadFile(sp.TargetBin)
	require.NoError(t, err)

	hooks := schema3Hooks(sp.ConfigPath, sp.DataDir)
	var out bytes.Buffer
	opts := updater.Options{
		CurrentVersion: version,
		TargetVersion:  sp.TargetVersion,
		HealthURL:      "http://" + sp.Addr + "/healthz",
		ReadyTimeout:   60 * time.Second,
		DataDir:        sp.DataDir,
		InstallBin:     sp.InstallBin,
		StagingDir:     sp.StagingDir,
		AssetBase:      sp.AssetBase,
		Context:        context.Background(),
		Preflight: func(ctx context.Context) (updater.PreflightResult, error) {
			return wholeStorePreflight(ctx, sp.DataDir)
		},
		TargetPreflight: func(ctx context.Context, targetBin string) (updater.PreflightResult, error) {
			return targetBinaryPreflight(ctx, targetBin, sp.DataDir)
		},
		Stdout:        &out,
		TargetSchema:  binarySchemaVersion,
		CurrentSchema: schema.SchemaVersion,
		Schema3:       hooks,
		Service:       svc,
	}
	require.Equal(t, 2, schema.SchemaVersion, "released updater must be the schema-2 waypoint")

	// The released updater calls VerifyReadiness with the target daemon healthy
	// and the transaction still pending (before the released finish()). A
	// mutation at that moment must be refused by the target's readiness gate.
	pendingMutations := 0
	verify := hooks.VerifyReadiness
	hooks.VerifyReadiness = func(ctx context.Context, ev updater.ReadinessEvidence) (bool, error) {
		code, body, err := rwMutate(sp.Addr)
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, code, "mutation while the schema-3 transaction is pending must be refused: %s", body)
		pendingMutations++
		return verify(ctx, ev)
	}

	if sp.Scenario == "readiness-failure-rolls-back" {
		hooks.VerifyReadiness = func(ctx context.Context, ev updater.ReadinessEvidence) (bool, error) {
			code, body, err := rwMutate(sp.Addr)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, code, "pending mutation: %s", body)
			pendingMutations++
			// Real readiness first (proves the target really owns the txn), then
			// force the failure that the released rollback must survive.
			ok, err := execVerifyReadiness(ctx, ev)
			require.NoError(t, err)
			require.True(t, ok)
			return false, nil
		}
		_, err := updater.Apply(opts)
		require.Error(t, err, "forced readiness failure must fail the update:\n%s", out.String())
		require.Contains(t, out.String(), "rolling back to the paired old binary and data")
		rwWaitHealthy(t, svc, sp.Addr, "12.1.0", 2)
		require.True(t, svc.running(), "restored old daemon must actually be running")
		require.Equal(t, 1, pendingMutations)
		require.NotEqual(t, originalPID, svc.pids[len(svc.pids)-1], "old daemon must have been restarted after rollback")
		installed, err := os.ReadFile(sp.InstallBin)
		require.NoError(t, err)
		require.Equal(t, waypointBytes, installed, "rollback restores the exact waypoint binary")
		l, err := schema.Load(sp.DataDir)
		require.NoError(t, err)
		require.Equal(t, 2, l.SchemaVersion)
		require.Nil(t, l.InProgress)
		return
	}

	res, err := updater.Apply(opts)
	require.NoError(t, err, "released updater.Apply must complete against the target:\n%s\ndaemon log:\n%s", out.String(), svc.log.String())
	require.True(t, res.Updated)
	require.False(t, res.RolledBack)
	rwWaitHealthy(t, svc, sp.Addr, sp.TargetVersion, 3)
	require.Equal(t, 1, pendingMutations)
	// Gate release may lag finalization by a poll interval; measure how long.
	gateStart := time.Now()
	var code int
	var body string
	for ; time.Since(gateStart) < 30*time.Second; time.Sleep(250 * time.Millisecond) {
		code, body, err = rwMutate(sp.Addr)
		require.NoError(t, err)
		if code != http.StatusServiceUnavailable {
			break
		}
	}
	t.Logf("readiness gate released %s after Apply returned (status %d)", time.Since(gateStart), code)
	require.NotEqual(t, http.StatusServiceUnavailable, code, "after finalize the daemon must not stay readiness-gated: %s\ndaemon log:\n%s", body, svc.log.String())
	installed, err := os.ReadFile(sp.InstallBin)
	require.NoError(t, err)
	require.Equal(t, targetBytes, installed)
	l, err := schema.Load(sp.DataDir)
	require.NoError(t, err)
	require.Equal(t, 3, l.SchemaVersion)
	require.Nil(t, l.InProgress, "released finish() must clear the pending transaction")

	if sp.Scenario != "apply-then-rollback" {
		return
	}
	m := regexp.MustCompile("--transaction (\\S+)").FindStringSubmatch(out.String())
	require.Len(t, m, 2, "released updater did not announce its transaction id:\n%s", out.String())
	rb, err := updater.Rollback(updater.RollbackOptions{
		DataDir: sp.DataDir, InstallBin: sp.InstallBin, HealthURL: opts.HealthURL, ReadyTimeout: 60 * time.Second,
		Force: true, Service: svc, TransactionID: m[1], AcceptDataLoss: true, Stdout: &out, Stderr: &out,
	})
	require.NoError(t, err, "released rollback failed:\n%s\ndaemon log:\n%s", out.String(), svc.log.String())
	require.True(t, rb.DataRestored)
	rwWaitHealthy(t, svc, sp.Addr, "12.1.0", 2)
	require.True(t, svc.running(), "restored old daemon must actually be running")
	installed, err = os.ReadFile(sp.InstallBin)
	require.NoError(t, err)
	require.Equal(t, waypointBytes, installed, "rollback restores the exact waypoint binary")
	l, err = schema.Load(sp.DataDir)
	require.NoError(t, err)
	require.Equal(t, 2, l.SchemaVersion)
}
`
