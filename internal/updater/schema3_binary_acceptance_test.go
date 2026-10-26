package updater

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/require"
)

// This test verifies the release boundary: the old published binary refuses a
// schema-3 generation before opening it, while the cutover target advertises
// schema 3. The full update/rollback protocol is exercised by the transaction
// acceptance tests with the approved waypoint.
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
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", "--output="+archive, "v11.7.0")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "actual release tag v11.7.0 is required; %s", out)
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
		{"published-tag", oldSource, "11.7.0", 2, true}, {"working-target", root, "cutover-boundary-test", 3, false},
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
}
