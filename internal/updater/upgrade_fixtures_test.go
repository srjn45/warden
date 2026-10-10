package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/repair"
	"github.com/srjn45/warden/internal/schema"
)

func fixtureBaseDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("testdata", "fixtures")
	// If testdata/fixtures doesn't exist yet, generate it
	if _, err := os.Stat(filepath.Join(dir, "v9.25.0-legacy")); os.IsNotExist(err) {
		require.NoError(t, GenerateUpgradeFixtures(dir))
	}
	return dir
}

// computeTreeHash returns a map of relative file path -> SHA256 hex digest for all non-transient files in dir.
func computeTreeHash(t *testing.T, dir string) map[string]string {
	t.Helper()
	hashes := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "backups" || strings.HasPrefix(base, ".") || base == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		// Skip temporary or backup files
		if strings.HasPrefix(filepath.Base(p), ".") || strings.Contains(filepath.ToSlash(rel), "backups/") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		_, err = io.Copy(h, f)
		if err != nil {
			return err
		}
		hashes[filepath.ToSlash(rel)] = fmt.Sprintf("%x", h.Sum(nil))
		return nil
	})
	require.NoError(t, err)
	return hashes
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o755))
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	require.NoError(t, err)
}

func assertByteIdentical(t *testing.T, expectedHashes map[string]string, dir string) {
	t.Helper()
	actualHashes := computeTreeHash(t, dir)
	require.Equal(t, len(expectedHashes), len(actualHashes), "file count must match")
	for rel, expectedHash := range expectedHashes {
		actualHash, exists := actualHashes[rel]
		require.True(t, exists, "file %s must exist in restored directory", rel)
		require.Equal(t, expectedHash, actualHash, "file %s must be byte-identical", rel)
	}
}

func makeTargetSchema(v int) func(context.Context, string) (int, error) {
	return func(context.Context, string) (int, error) { return v, nil }
}

func TestUpgradeFixtures(t *testing.T) {
	fixturesDir := fixtureBaseDir(t)

	fixtures := []struct {
		name          string
		initialVer    string
		hasBadHistory bool
	}{
		{
			name:          "v9.25.0-legacy",
			initialVer:    "9.25.0",
			hasBadHistory: false,
		},
		{
			name:          "v9.26.0-waypoint",
			initialVer:    "9.26.0",
			hasBadHistory: false,
		},
		{
			name:          "v9.27.0-bad-history",
			initialVer:    "9.27.0",
			hasBadHistory: true,
		},
	}

	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := filepath.Join(fixturesDir, tc.name)
			workDir := t.TempDir()
			dataDir := filepath.Join(workDir, "data")
			copyDir(t, srcDir, dataDir)

			binPath := filepath.Join(workDir, "warden")
			stagedPath := filepath.Join(workDir, "staged")
			require.NoError(t, os.WriteFile(binPath, []byte("warden-"+tc.initialVer), 0o755))
			require.NoError(t, os.WriteFile(stagedPath, []byte("warden-9.29.0"), 0o755))

			if tc.hasBadHistory {
				// 1. Verify preflight detects bad history and suggests repair
				findings, err := migrate.VerifyAllStores(context.Background(), dataDir)
				require.NoError(t, err)
				require.NotEmpty(t, findings)
				foundRepairable := false
				for _, f := range findings {
					if f.Severity == migrate.SeverityRepairable {
						foundRepairable = true
						require.Equal(t, "warden repair all --resolve-history=live-wins", f.Command)
					}
				}
				require.True(t, foundRepairable, "bad history must be classified as repairable")

				// 2. Run repair all with live-wins
				rep, err := repair.RepairAll(context.Background(), repair.Options{
					DataDir:        dataDir,
					ResolveHistory: "live-wins",
					Version:        "9.29.0",
				})
				require.NoError(t, err)
				require.True(t, rep.Clean)
			}

			// Capture pristine state hashes before upgrade transaction
			beforeHashes := computeTreeHash(t, dataDir)

			// Setup transaction collaborators
			svc := &fakeSvc{}
			clk := newFakeClock()
			out := &bytes.Buffer{}
			probe := &fakeProbe{fn: func() (Health, error) {
				return Health{Status: "ok", Version: "9.29.0", SchemaVersion: 1}, nil
			}}

			opts := Options{
				CurrentVersion: tc.initialVer,
				TargetVersion:  "9.29.0",
				InstallBin:     binPath,
				DataDir:        dataDir,
				Stdout:         out,
				ReadyTimeout:   10 * time.Second,
				TargetSchema:   makeTargetSchema(1),
				Migrate: func() error {
					// Migration step: stamp or advance schema ledger
					l, _ := schema.Load(dataDir)
					if l == nil {
						l = &schema.Ledger{SchemaVersion: 1, BinaryVersion: "9.29.0"}
					} else {
						l.BinaryVersion = "9.29.0"
					}
					l.History = append(l.History, schema.HistoryEntry{
						From:      l.SchemaVersion,
						To:        1,
						Migration: "upgrade-to-9.29.0",
						At:        time.Now().UTC(),
					})
					l.InProgress = nil
					return schema.Save(dataDir, l)
				},
			}

			tx := &txn{
				opts:   opts,
				svc:    svc,
				probe:  probe,
				inst:   fsInstaller{bin: binPath},
				clock:  clk,
				target: "9.29.0",
			}

			tx.capture(context.Background())
			rb, err := tx.apply(context.Background(), stagedPath)
			require.NoError(t, err)
			require.False(t, rb, "upgrade succeeded without rollback")

			// Assertion: clean migration
			led, err := schema.Load(dataDir)
			require.NoError(t, err)
			require.Equal(t, 1, led.SchemaVersion)
			require.Equal(t, "9.29.0", led.BinaryVersion)
			require.Nil(t, led.InProgress)

			// Assertion: verify clean
			findings, err := migrate.VerifyAllStores(context.Background(), dataDir)
			require.NoError(t, err)
			for _, f := range findings {
				require.NotEqual(t, migrate.SeverityBlocking, f.Severity)
				require.NotEqual(t, migrate.SeverityRepairable, f.Severity)
			}

			// Assertion: daemon healthy
			h, err := probe.Probe(context.Background())
			require.NoError(t, err)
			require.Equal(t, "ok", h.Status)
			require.Equal(t, "9.29.0", h.Version)
			require.Equal(t, 1, h.SchemaVersion)

			// Assertion: snapshot & rollback restores byte-identical data
			t.Run("RollbackRestoresByteIdenticalData", func(t *testing.T) {
				rbDir := t.TempDir()
				rbDataDir := filepath.Join(rbDir, "data")
				copyDir(t, srcDir, rbDataDir)

				if tc.hasBadHistory {
					_, err := repair.RepairAll(context.Background(), repair.Options{
						DataDir:        rbDataDir,
						ResolveHistory: "live-wins",
						Version:        "9.29.0",
					})
					require.NoError(t, err)
				}

				preHashes := computeTreeHash(t, rbDataDir)

				rbBinPath := filepath.Join(rbDir, "warden")
				rbStagedPath := filepath.Join(rbDir, "staged")
				require.NoError(t, os.WriteFile(rbBinPath, []byte("warden-"+tc.initialVer), 0o755))
				require.NoError(t, os.WriteFile(rbStagedPath, []byte("warden-9.29.0"), 0o755))

				failOpts := Options{
					CurrentVersion: tc.initialVer,
					TargetVersion:  "9.29.0",
					InstallBin:     rbBinPath,
					DataDir:        rbDataDir,
					Stdout:         &bytes.Buffer{},
					ReadyTimeout:   5 * time.Second,
					Migrate: func() error {
						// Mutate data then fail
						_ = os.WriteFile(filepath.Join(rbDataDir, "context", "context", "seg_000001.ndjson"), []byte("corrupted mid-upgrade"), 0o644)
						return errors.New("simulated migration failure")
					},
				}

				failTx := &txn{
					opts:   failOpts,
					svc:    &fakeSvc{},
					probe:  &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: tc.initialVer}, nil }},
					inst:   fsInstaller{bin: rbBinPath},
					clock:  newFakeClock(),
					target: "9.29.0",
				}

				failTx.capture(context.Background())
				rolledBack, applyErr := failTx.apply(context.Background(), rbStagedPath)
				require.Error(t, applyErr)
				require.True(t, rolledBack)

				// Verify byte-identical restoration
				assertByteIdentical(t, preHashes, rbDataDir)
			})
			_ = beforeHashes
		})
	}
}

func TestMultiHopUpgrade(t *testing.T) {
	// Oldest supported version: v9.25.0 -> waypoint: v9.26.0 -> latest: v9.29.0
	currVer := "9.25.0"

	manifests := []Manifest{
		{
			Version:        "9.25.0",
			SchemaVersion:  1,
			MinUpgradeFrom: "9.22.0",
		},
		{
			Version:        "9.26.0",
			SchemaVersion:  1,
			Waypoint:       true,
			MinUpgradeFrom: "9.25.0",
			Notes:          []string{"Waypoint: database format consolidation"},
		},
		{
			Version:        "9.27.0",
			SchemaVersion:  1,
			MinUpgradeFrom: "9.26.0",
		},
		{
			Version:        "9.28.0",
			SchemaVersion:  1,
			MinUpgradeFrom: "9.26.0",
		},
		{
			Version:        "9.29.0",
			SchemaVersion:  1,
			MinUpgradeFrom: "9.26.0", // Direct upgrade requires at least 9.26.0 (waypoint)
			Notes:          []string{"Latest release"},
		},
	}

	targetManifest := manifests[len(manifests)-1]
	plan, err := ComputePath(currVer, 1, targetManifest, manifests)
	require.NoError(t, err)

	// Plan must not be direct; it must route through waypoint 9.26.0
	require.False(t, plan.Direct)
	require.Len(t, plan.Hops, 2)
	require.Equal(t, "9.26.0", plan.Hops[0].Manifest.Version)
	require.Equal(t, "9.29.0", plan.Hops[1].Manifest.Version)

	// Execute each hop in order against fixture data
	fixturesDir := fixtureBaseDir(t)
	workDir := t.TempDir()
	dataDir := filepath.Join(workDir, "data")
	copyDir(t, filepath.Join(fixturesDir, "v9.25.0-legacy"), dataDir)

	binPath := filepath.Join(workDir, "warden")
	require.NoError(t, os.WriteFile(binPath, []byte("bin-9.25.0"), 0o755))

	svc := &fakeSvc{}
	clk := newFakeClock()

	currentVerState := currVer
	for hopIdx, hop := range plan.Hops {
		stagedBin := filepath.Join(workDir, fmt.Sprintf("staged-%s", hop.Manifest.Version))
		require.NoError(t, os.WriteFile(stagedBin, []byte("bin-"+hop.Manifest.Version), 0o755))

		probe := &fakeProbe{fn: func() (Health, error) {
			return Health{Status: "ok", Version: hop.Manifest.Version, SchemaVersion: hop.Manifest.SchemaVersion}, nil
		}}

		targetSchemaVal := hop.Manifest.SchemaVersion
		opts := Options{
			CurrentVersion: currentVerState,
			TargetVersion:  hop.Manifest.Version,
			InstallBin:     binPath,
			DataDir:        dataDir,
			Stdout:         &bytes.Buffer{},
			ReadyTimeout:   5 * time.Second,
			TargetSchema:   makeTargetSchema(targetSchemaVal),
			Migrate: func() error {
				l, _ := schema.Load(dataDir)
				if l == nil {
					l = &schema.Ledger{SchemaVersion: 1}
				}
				l.BinaryVersion = hop.Manifest.Version
				l.History = append(l.History, schema.HistoryEntry{
					From:      l.SchemaVersion,
					To:        hop.Manifest.SchemaVersion,
					Migration: fmt.Sprintf("hop-%d-%s", hopIdx+1, hop.Manifest.Version),
					At:        time.Now().UTC(),
				})
				l.InProgress = nil
				return schema.Save(dataDir, l)
			},
		}

		tx := &txn{
			opts:   opts,
			svc:    svc,
			probe:  probe,
			inst:   fsInstaller{bin: binPath},
			clock:  clk,
			target: hop.Manifest.Version,
		}

		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), stagedBin)
		require.NoError(t, err)
		require.False(t, rb)

		currentVerState = hop.Manifest.Version
	}

	// Final assertions after all hops
	led, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, led.SchemaVersion)
	require.Equal(t, "9.29.0", led.BinaryVersion)
	require.Len(t, led.History, 2)

	findings, err := migrate.VerifyAllStores(context.Background(), dataDir)
	require.NoError(t, err)
	require.Empty(t, findings)
}

func TestTxnFault_AllSteps(t *testing.T) {
	t.Run("Step1_PreflightFails", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{}
		opts := Options{
			CurrentVersion: "1.0.0",
			TargetVersion:  "2.0.0",
			InstallBin:     bin,
			Stdout:         &bytes.Buffer{},
			TargetPreflight: func(ctx context.Context, targetBin string) (PreflightResult, error) {
				return PreflightResult{
					Blockers:      []string{"store 'context' has corrupt segments"},
					RepairCommand: "warden repair all --resolve-history=live-wins",
				}, nil
			},
		}

		tx := &txn{opts: opts, svc: svc, inst: fsInstaller{bin: bin}, clock: newFakeClock(), target: "2.0.0"}
		tx.capture(context.Background())
		err := tx.targetPreflight(context.Background(), staged)
		require.Error(t, err)
		var pe *PreflightError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, "warden repair all --resolve-history=live-wins", pe.RepairCommand)
		require.Equal(t, 0, svc.stops, "daemon must not be stopped on preflight failure")
	})

	t.Run("Step2_StopDaemonFails", func(t *testing.T) {
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{stopErr: func() error { return errors.New("systemctl: unit cannot be stopped") }}
		probe := &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: "1.0.0"}, nil }}

		tx := &txn{
			opts:   Options{CurrentVersion: "1.0.0", TargetVersion: "2.0.0", InstallBin: bin, DataDir: dataDir, Stdout: &bytes.Buffer{}},
			svc:    svc,
			probe:  probe,
			inst:   fsInstaller{bin: bin},
			clock:  newFakeClock(),
			target: "2.0.0",
		}
		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), staged)
		require.Error(t, err)
		require.False(t, rb)
		require.Contains(t, err.Error(), "stop daemon")
	})

	t.Run("Step3_SnapshotFails", func(t *testing.T) {
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		backupsDir := filepath.Join(dataDir, "backups")
		require.NoError(t, os.MkdirAll(backupsDir, 0o500))
		defer os.Chmod(backupsDir, 0o700)

		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{}
		probe := &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: "1.0.0"}, nil }}

		tx := &txn{
			opts:   Options{CurrentVersion: "1.0.0", TargetVersion: "2.0.0", InstallBin: bin, DataDir: dataDir, Stdout: &bytes.Buffer{}},
			svc:    svc,
			probe:  probe,
			inst:   fsInstaller{bin: bin},
			clock:  newFakeClock(),
			target: "2.0.0",
		}
		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), staged)
		require.Error(t, err)
		require.False(t, rb)
		require.Contains(t, err.Error(), "snapshot data")
		require.Equal(t, 1, svc.restarts, "daemon must be restarted after snapshot failure")
	})

	t.Run("Step4_BinarySwapFails", func(t *testing.T) {
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))

		svc := &fakeSvc{}
		probe := &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: "1.0.0"}, nil }}

		tx := &txn{
			opts:   Options{CurrentVersion: "1.0.0", TargetVersion: "2.0.0", InstallBin: bin, DataDir: dataDir, Stdout: &bytes.Buffer{}},
			svc:    svc,
			probe:  probe,
			inst:   fsInstaller{bin: bin},
			clock:  newFakeClock(),
			target: "2.0.0",
		}
		tx.capture(context.Background())
		// Swap non-existent staged path
		rb, err := tx.apply(context.Background(), filepath.Join(dir, "non-existent"))
		require.Error(t, err)
		require.False(t, rb)
		require.Equal(t, 1, svc.restarts)
	})

	t.Run("Step5_MidMigrationCrash", func(t *testing.T) {
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "context", "context"), 0o755))
		seg := filepath.Join(dataDir, "context", "context", "seg_000001.ndjson")
		require.NoError(t, os.WriteFile(seg, []byte("pristine"), 0o644))
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{}
		probe := &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: "1.0.0"}, nil }}

		opts := Options{
			CurrentVersion: "1.0.0",
			TargetVersion:  "2.0.0",
			InstallBin:     bin,
			DataDir:        dataDir,
			Stdout:         &bytes.Buffer{},
			ReadyTimeout:   5 * time.Second,
			Migrate: func() error {
				_ = os.WriteFile(seg, []byte("corrupted mid-flight"), 0o644)
				return errors.New("unhandled panic / crash mid-migration")
			},
		}

		tx := &txn{opts: opts, svc: svc, probe: probe, inst: fsInstaller{bin: bin}, clock: newFakeClock(), target: "2.0.0"}
		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), staged)
		require.Error(t, err)
		require.True(t, rb)

		// Restored pristine data
		restored, rErr := os.ReadFile(seg)
		require.NoError(t, rErr)
		require.Equal(t, "pristine", string(restored))
	})

	t.Run("Step6_HealthCheckFailsAfterRestart", func(t *testing.T) {
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{}
		probeCalls := 0
		probe := &fakeProbe{fn: func() (Health, error) {
			probeCalls++
			if probeCalls == 1 {
				return Health{Status: "ok", Version: "1.0.0"}, nil
			}
			if svc.count() == 1 {
				// Daemon fails health check
				return Health{Status: "crash", Version: "2.0.0"}, nil
			}
			return Health{Status: "ok", Version: "1.0.0"}, nil
		}}

		opts := Options{
			CurrentVersion: "1.0.0",
			TargetVersion:  "2.0.0",
			InstallBin:     bin,
			DataDir:        dataDir,
			Stdout:         &bytes.Buffer{},
			ReadyTimeout:   1 * time.Second,
		}

		tx := &txn{opts: opts, svc: svc, probe: probe, inst: fsInstaller{bin: bin}, clock: newFakeClock(), target: "2.0.0"}
		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), staged)
		require.Error(t, err)
		require.True(t, rb)
		require.Contains(t, err.Error(), "restored v1.0.0")
	})

	t.Run("Step7_RollbackFailureDiagnostics", func(t *testing.T) {
		// When rollback itself encounters an error, verify diagnostic details are preserved
		dir := t.TempDir()
		dataDir := filepath.Join(dir, "data")
		bin := filepath.Join(dir, "bin")
		staged := filepath.Join(dir, "staged")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		require.NoError(t, os.WriteFile(bin, []byte("v1"), 0o755))
		require.NoError(t, os.WriteFile(staged, []byte("v2"), 0o755))

		svc := &fakeSvc{restartErr: func(int) error { return errors.New("cannot restart service") }}
		probe := &fakeProbe{fn: func() (Health, error) { return Health{Status: "ok", Version: "1.0.0"}, nil }}

		opts := Options{
			CurrentVersion: "1.0.0",
			TargetVersion:  "2.0.0",
			InstallBin:     bin,
			DataDir:        dataDir,
			Stdout:         &bytes.Buffer{},
			Migrate: func() error {
				return errors.New("migration failed")
			},
		}

		tx := &txn{opts: opts, svc: svc, probe: probe, inst: fsInstaller{bin: bin}, clock: newFakeClock(), target: "2.0.0"}
		tx.capture(context.Background())
		rb, err := tx.apply(context.Background(), staged)
		require.Error(t, err)
		require.False(t, rb, "rollback reported incomplete due to restart error")

		var fe *FailureError
		require.ErrorAs(t, err, &fe)
		require.Error(t, fe.Rollback)
		var re *RollbackError
		require.ErrorAs(t, fe.Rollback, &re)
		require.NotEmpty(t, re.SnapshotDir)
		require.Equal(t, dataDir, re.DataDir)
		require.Contains(t, fe.Rollback.Error(), "restart previous service")
	})
}
