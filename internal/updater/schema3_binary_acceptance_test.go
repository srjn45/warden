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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This test verifies the release boundary against the approved schema-2
// v12.1.0 waypoint: that exact released binary refuses a schema-3 generation before opening it,
// while the cutover target advertises schema 3.
// Run through `wd check schema3-binary-boundary` so expensive isolated builds are
// explicit and ordinary package unit tests do not recursively build themselves.
func TestSchema3ActualBinaryBoundary(t *testing.T) {
	if os.Getenv("WARDEN_SCHEMA3_BINARY_ACCEPTANCE") != "1" {
		t.Skip("run wd check schema3-binary-boundary for isolated actual binary builds")
	}
	if runtime.GOOS == "windows" {
		t.Skip("release tooling distributes Linux and Darwin binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	oldSource := t.TempDir()
	archive := filepath.Join(t.TempDir(), "release.tar")
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", "--output="+archive, "5fceb280")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "released v12.1.0 waypoint 5fceb280 is required; %s", out)
	out, err = exec.CommandContext(ctx, "tar", "-xf", archive, "-C", oldSource).CombinedOutput()
	require.NoError(t, err, "%s", out)
	// The old release built UI assets before compiling; an empty isolated embed
	// placeholder is enough for this pre-store startup-refusal test.
	require.NoError(t, os.MkdirAll(filepath.Join(oldSource, "web", "dist"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(oldSource, "web", "dist", ".gitkeep"), nil, 0600))
	for _, source := range []struct {
		name, dir, version string
		schema             int
		mustRefuse         bool
	}{
		{"approved-waypoint", oldSource, "12.1.0", 2, true}, {"working-target", root, "cutover-boundary-test", 3, false},
	} {
		t.Run(source.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "warden")
			build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/srjn45/warden/internal/cli.version="+source.version, "-o", bin, "./cmd/warden")
			build.Dir = source.dir
			out, err := build.CombinedOutput()
			require.NoError(t, err, "%s", out)
			out, err = exec.CommandContext(ctx, bin, "version", "--json").CombinedOutput()
			require.NoError(t, err, "%s", out)
			var info struct {
				Version string `json:"version"`
				Schema  int    `json:"schema_version"`
			}
			require.NoError(t, json.Unmarshal(out, &info))
			require.Equal(t, source.version, strings.TrimPrefix(info.Version, "v"))
			require.Equal(t, source.schema, info.Schema)
			if !source.mustRefuse {
				return
			}
			data := t.TempDir()
			require.NoError(t, schema.Save(data, &schema.Ledger{SchemaVersion: 3, BinaryVersion: "schema3-fixture"}))
			require.NoError(t, os.MkdirAll(filepath.Join(data, "warden-db"), 0700))
			sentinel := filepath.Join(data, "warden-db", "must-not-open")
			require.NoError(t, os.WriteFile(sentinel, []byte("fixture generation"), 0600))
			ledgerBefore, err := os.ReadFile(filepath.Join(data, schema.FileName))
			require.NoError(t, err)
			cfg := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+data+"\naddr: 127.0.0.1:0\n"), 0600))
			start := exec.CommandContext(ctx, bin, "--config", cfg, "daemon")
			out, err = start.CombinedOutput()
			require.Error(t, err, "schema-2 executable must refuse schema-3 startup")
			require.Contains(t, string(out), "schema 3")
			ledgerAfter, err := os.ReadFile(filepath.Join(data, schema.FileName))
			require.NoError(t, err)
			require.Equal(t, ledgerBefore, ledgerAfter)
			body, err := os.ReadFile(sentinel)
			require.NoError(t, err)
			require.Equal(t, "fixture generation", string(body))
			entries, err := os.ReadDir(filepath.Join(data, "warden-db"))
			require.NoError(t, err)
			require.Len(t, entries, 1, "refused binary must not open a collection")
		})
	}
	testSchema3WaypointUpdateStartAndRollback(t)
}

// TestSchema3WaypointUpdateStartAndRollback uses binaries built from the
// approved waypoint and this checkout. It exercises the production target's
// authenticated readiness command against the promoted generation, starts the
// target daemon on that generation, and then restores the paired waypoint
// binary and schema-2 data generation.
func testSchema3WaypointUpdateStartAndRollback(t *testing.T) {
	if os.Getenv("WARDEN_SCHEMA3_BINARY_ACCEPTANCE") != "1" {
		t.Skip("run wd check schema3-binary-boundary for isolated actual binary builds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	waypointSource := t.TempDir()
	archive := filepath.Join(t.TempDir(), "waypoint.tar")
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", "--output="+archive, "5fceb280")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.CommandContext(ctx, "tar", "-xf", archive, "-C", waypointSource).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.NoError(t, os.MkdirAll(filepath.Join(waypointSource, "web", "dist"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(waypointSource, "web", "dist", ".gitkeep"), nil, 0600))
	build := func(dir, version string) string {
		bin := filepath.Join(t.TempDir(), "warden")
		buildCmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/srjn45/warden/internal/cli.version="+version, "-o", bin, "./cmd/warden")
		buildCmd.Dir = dir
		out, err := buildCmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return bin
	}
	waypoint := build(waypointSource, "12.1.0")
	target := build(root, "13.0.0")

	tr, installed, staged := schema3Fixture(t)
	for _, copy := range [][2]string{{waypoint, installed}, {target, staged}} {
		body, err := os.ReadFile(copy[0])
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(copy[1], body, 0755))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	require.NoError(t, os.WriteFile(filepath.Join(tr.dir, "config.yaml"), []byte(fmt.Sprintf("data_dir: %s\naddr: %s\n", tr.dir, addr)), 0600))

	beginSchema3Fixture(t, tr, installed, staged)
	require.NoError(t, tr.promote(ctx))
	_, err = (fsInstaller{bin: installed}).Swap(staged)
	require.NoError(t, err)
	require.NoError(t, tr.handoff())

	daemon := exec.CommandContext(ctx, installed, "--config", filepath.Join(tr.dir, "config.yaml"), "daemon")
	var daemonLog bytes.Buffer
	daemon.Stdout = &daemonLog
	daemon.Stderr = &daemonLog
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		if daemon.Process != nil {
			_ = daemon.Process.Kill()
			_, _ = daemon.Process.Wait()
		}
	})
	healthURL := "http://" + addr + "/healthz"
	var h Health
	ready := assert.Eventually(t, func() bool {
		resp, err := http.Get(healthURL) // #nosec G107 -- loopback test daemon
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(resp.Body).Decode(&h) == nil && h.Status == "ok" && h.SchemaVersion == 3 && strings.TrimPrefix(h.Version, "v") == "13.0.0"
	}, 20*time.Second, 100*time.Millisecond)
	require.True(t, ready, "target daemon output:\n%s", daemonLog.String())

	out, err = exec.CommandContext(ctx, installed, "migrate", "--verify-ownership", "--data-dir", tr.dir, "--transaction", tr.id, "--target-sha", tr.manifest.TargetBinaryHash, "--manifest-sha", tr.manifestHash).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.NoError(t, tr.finish(ctx, h, true))
	require.NoError(t, daemon.Process.Kill())
	_, _ = daemon.Process.Wait()

	svc := &pairedRestoreService{}
	probe := &fakeProbe{fn: func() (Health, error) {
		if svc.stopped {
			return Health{}, fmt.Errorf("stopped")
		}
		return Health{Status: "ok", Version: "12.1.0", SchemaVersion: 2}, nil
	}}
	result, err := Rollback(RollbackOptions{DataDir: tr.dir, InstallBin: installed, TransactionID: tr.id, AcceptDataLoss: true, Service: svc, Prober: probe, Clock: newFakeClock()})
	require.NoError(t, err)
	require.True(t, result.DataRestored)
	ledger, err := schema.Load(tr.dir)
	require.NoError(t, err)
	require.Equal(t, 2, ledger.SchemaVersion)
	got, err := os.ReadFile(installed)
	require.NoError(t, err)
	want, err := os.ReadFile(waypoint)
	require.NoError(t, err)
	require.Equal(t, want, got, "rollback restored the exact waypoint binary")
}
