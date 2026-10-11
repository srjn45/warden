package storage

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/audit/auditstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/wardendb"
)

// sharedDir returns a data dir that selects schema-2 shared mode (the
// non-user-facing warden-db marker).
func sharedDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, os.MkdirAll(wardendb.Dir(d), 0o700))
	return d
}

func openMode(t *testing.T, shared bool) (*Stores, string) {
	t.Helper()
	d := t.TempDir()
	if shared {
		d = sharedDir(t)
	}
	s, err := Open(d)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.Equal(t, shared, s.DB != nil, "mode selection")
	return s, d
}

func TestModeSelection(t *testing.T) {
	compat, _ := openMode(t, false)
	require.Nil(t, compat.DB)
	shared, _ := openMode(t, true)
	require.NotNil(t, shared.DB)
}

// The same CLI/API-facing operations must behave identically in schema-1
// compat mode and in unified mode.
func TestStoreBehaviorParityAcrossModes(t *testing.T) {
	ctx := context.Background()
	for name, shared := range map[string]bool{"compat": false, "unified": true} {
		t.Run(name, func(t *testing.T) {
			s, _ := openMode(t, shared)

			require.NoError(t, s.Agents.Create(ctx, &agentstore.Agent{ID: "a1", Name: "worker", Status: store.StatusWorking}))
			got, err := s.Agents.GetByNameOrID(ctx, "worker")
			require.NoError(t, err)
			require.Equal(t, "a1", got.ID)
			require.NoError(t, s.Agents.UpdateStatus(ctx, "a1", store.StatusIdle))
			list, err := s.Agents.List(ctx)
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, store.StatusIdle, list[0].Status)

			_, err = s.Context.Set("k", "v1", "me")
			require.NoError(t, err)
			e, err := s.Context.Append("k", "v2", ",", "me")
			require.NoError(t, err)
			require.Equal(t, "v1,v2", e.Value)
			_, err = s.Context.CompareAndSet("k", "stale", "x", "me")
			require.Error(t, err)

			base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			s.Audit.Log(audit.Event{Time: base, Action: "spawn", Target: "a1"})
			s.Audit.Log(audit.Event{Time: base.Add(time.Second), Action: "stop", Target: "a1"})
			evs, err := s.Audit.Read(audit.Filter{Action: "stop"})
			require.NoError(t, err)
			require.Len(t, evs, 1)
			require.Equal(t, "a1", evs[0].Target)

			require.NoError(t, s.Metrics.Record(metrics.Sample{TakenAt: time.Now()}))
			hist, err := s.Metrics.History(time.Now().Add(-time.Hour), 10)
			require.NoError(t, err)
			require.Len(t, hist, 1)
		})
	}
}

// Large transcript artifacts stay external to the database in both modes, are
// referenced from the snapshot record, and reconcile correctly.
func TestSnapshotLargeArtifactReconcileAcrossModes(t *testing.T) {
	body := strings.Repeat("0123456789abcdef", 256*1024) // 4 MiB
	for name, shared := range map[string]bool{"compat": false, "unified": true} {
		t.Run(name, func(t *testing.T) {
			s, dataDir := openMode(t, shared)
			require.NoError(t, s.Snapshots.Put(&snapshot.Snapshot{ID: "snap-big", SessionID: "a1"}, body))

			rec, err := s.Snapshots.Get("snap-big")
			require.NoError(t, err)
			require.Len(t, rec.Artifacts, 1)
			ref := rec.Artifacts[0]
			require.Equal(t, snapshot.ArtifactTranscript, ref.Kind)
			require.EqualValues(t, len(body), ref.Size)
			require.Len(t, ref.SHA256, 64)

			// The record itself must not inline the blob: the transcript lives in a
			// file beside the store, never inside warden-db.
			if shared {
				require.NoError(t, filepath.Walk(wardendb.Dir(dataDir), func(p string, fi os.FileInfo, err error) error {
					require.NoError(t, err)
					require.Less(t, fi.Size(), int64(len(body)), "blob leaked into warden-db: %s", p)
					return nil
				}))
			}

			fs, err := s.Snapshots.Reconcile("snap-big")
			require.NoError(t, err)
			require.Len(t, fs, 1)
			require.True(t, fs[0].OK(), fs[0])

			// Truncate then corrupt same-size: reconcile reports, never repairs.
			full := filepath.Join(dataDir, filepath.FromSlash(ref.Path))
			require.NoError(t, os.Truncate(full, int64(len(body)-1)))
			fs, _ = s.Snapshots.Reconcile("snap-big")
			require.Equal(t, snapshot.ArtifactSizeMismatch, fs[0].Status)
			require.NoError(t, os.WriteFile(full, []byte(strings.Repeat("X", len(body))), 0o600))
			fs, _ = s.Snapshots.Reconcile("snap-big")
			require.Equal(t, snapshot.ArtifactHashMismatch, fs[0].Status)
			require.NoError(t, os.Remove(full))
			fs, _ = s.Snapshots.Reconcile("snap-big")
			require.Equal(t, snapshot.ArtifactMissing, fs[0].Status)
			still, err := s.Snapshots.Get("snap-big")
			require.NoError(t, err, "record must survive a missing artifact")
			require.Len(t, still.Artifacts, 1)
		})
	}
}

// Stores.Close in unified mode: every store/collection is retired before the
// shared handle, the handle closes once, the ownership lock is released, and
// repeated Close is a harmless no-op.
func TestUnifiedCloseOrderAndIdempotence(t *testing.T) {
	d := sharedDir(t)
	s, err := Open(d)
	require.NoError(t, err)

	var sawHandleErr error
	var sawAgentErr error
	require.NoError(t, s.DB.OnClose("probe", func() error {
		// Registered last => runs first: collections must still be usable and
		// the handle must not be closed yet.
		_, sawAgentErr = s.Agents.List(context.Background())
		_, sawHandleErr = s.DB.Handle() // "closing" => ErrClosed, not nil-handle panic
		return nil
	}))

	require.NoError(t, s.Close())
	require.NoError(t, sawAgentErr, "stores must still be open while closers run")
	require.ErrorIs(t, sawHandleErr, wardendb.ErrClosed)

	_, err = s.DB.Handle()
	require.ErrorIs(t, err, wardendb.ErrClosed)
	st, err := wardendb.Probe(d)
	require.NoError(t, err)
	require.False(t, st.Owned)

	require.NoError(t, s.Close(), "second Close must be a no-op")
	require.NoError(t, s.DB.Close())

	// The directory is cleanly reopenable: nothing was left holding it.
	s2, err := Open(d)
	require.NoError(t, err)
	require.NoError(t, s2.Close())
}

func TestCompatCloseReleasesEverythingOnce(t *testing.T) {
	d := t.TempDir()
	s, err := Open(d)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NotPanics(t, func() { _ = s.Close() })
	s2, err := Open(d)
	require.NoError(t, err, "compat locks must be released by Close")
	require.NoError(t, s2.Close())
}

// A second opener must be refused rather than silently opening the embedded
// database twice.
func TestNoDuplicateOpenWhileOwned(t *testing.T) {
	t.Run("unified", func(t *testing.T) {
		s, d := openMode(t, true)
		_, err := Open(d)
		require.Error(t, err)
		require.True(t, errors.Is(err, wardendb.ErrLocked), "got %v", err)
		_, err = wardendb.Open(d)
		require.ErrorIs(t, err, wardendb.ErrLocked)
		require.NotNil(t, s.DB)
	})
	t.Run("compat", func(t *testing.T) {
		_, d := openMode(t, false)
		s2, err := Open(d)
		if err == nil {
			_ = s2.Close()
		}
		require.Error(t, err, "compat layout must not allow a second concurrent owner")
	})
}

// Unified mode must put every durable store behind the one handle: no legacy
// per-store directories may appear next to warden-db.
func TestUnifiedModeCreatesOnlySharedDB(t *testing.T) {
	s, d := openMode(t, true)
	ents, err := os.ReadDir(d)
	require.NoError(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	// snapshots/ is the external transcript-artifact directory, not a store.
	require.Equal(t, []string{"snapshots", wardendb.DirName}, names, "legacy store paths created in unified mode")
	snaps, err := os.ReadDir(filepath.Join(d, "snapshots"))
	require.NoError(t, err)
	require.Empty(t, snaps)
	require.NoError(t, s.Close())
}

// Partial-open failure must release everything opened so far.
func TestPartialOpenFailureCleansUp(t *testing.T) {
	t.Run("compat", func(t *testing.T) {
		d := t.TempDir()
		// pipelines/ is a regular file => pipeline.NewStore fails after
		// agents/terminals/context/mailbox/knownprompts opened.
		require.NoError(t, os.WriteFile(filepath.Join(d, "pipelines"), []byte("x"), 0o600))
		_, err := Open(d)
		require.Error(t, err)
		require.NoError(t, os.Remove(filepath.Join(d, "pipelines")))
		s, err := Open(d)
		require.NoError(t, err, "earlier stores must have been closed on failure")
		require.NoError(t, s.Close())
	})
	t.Run("unified", func(t *testing.T) {
		d := sharedDir(t)
		s, err := Open(d)
		require.NoError(t, err)
		require.NoError(t, s.Close())
		// Corrupt the last-registered collection so the failure lands late,
		// after almost every other store was composed.
		seg := filepath.Join(wardendb.Dir(d), "audit_events", "seg_000001.ndjson")
		require.NoError(t, os.WriteFile(seg, []byte("{not json\n"), 0o600))
		_, err = Open(d)
		require.Error(t, err)
		st, perr := wardendb.Probe(d)
		require.NoError(t, perr)
		require.False(t, st.Owned, "failed Open leaked the ownership lock")
	})
}

// Every package's declared collections, composed by storage.Open, must be
// exactly the registered set, with no duplicates and catalogue membership.
func TestCollectionConfigParity(t *testing.T) {
	var specs []wardendb.Spec
	for _, f := range [][]wardendb.Spec{
		agentstore.CollectionSpecs(), terminalstore.CollectionSpecs(), ctxstore.CollectionSpecs(),
		mailbox.CollectionSpecs(), knownprompts.CollectionSpecs(), pipeline.CollectionSpecs(),
		snapshot.CollectionSpecs(), savings.CollectionSpecs(), spend.CollectionSpecs(),
		schedule.CollectionSpecs(), backendstore.CollectionSpecs(), backendusage.CollectionSpecs(),
		capacity.CollectionSpecs(), projectstore.CollectionSpecs(), planstore.CollectionSpecs(),
		planexport.CollectionSpecs(), metrics.CollectionSpecs(), autopilot.RunCollectionSpecs(),
		autopilotstore.CollectionSpecs(), auditstore.CollectionSpecs(),
	} {
		specs = append(specs, f...)
	}
	want := map[string]int{}
	cat := map[string]bool{}
	for _, n := range wardendb.Catalogue() {
		cat[n] = true
	}
	for _, sp := range specs {
		_, dup := want[sp.Name]
		require.False(t, dup, "collection %q declared by two packages", sp.Name)
		require.True(t, cat[sp.Name], "%q not in catalogue", sp.Name)
		want[sp.Name] = sp.Version
	}
	require.Contains(t, want, "autopilots")
	require.Contains(t, want, "autopilot_runs")
	for _, n := range []string{"audit_events", "metrics_samples", "snapshots"} {
		require.Contains(t, want, n)
	}

	s, d := openMode(t, true)
	infos, err := s.DB.Registered()
	require.NoError(t, err)
	got := map[string]int{}
	for _, i := range infos {
		got[i.Name] = i.Version
	}
	require.Equal(t, want, got)

	// autopilots and autopilot_runs stay distinct collections with their own
	// index sets.
	idx := func(coll string) []string {
		ents, err := os.ReadDir(filepath.Join(wardendb.Dir(d), coll))
		require.NoError(t, err)
		var out []string
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "sidx_") {
				out = append(out, e.Name())
			}
		}
		sort.Strings(out)
		return out
	}
	runs, defs := idx("autopilot_runs"), idx("autopilots")
	require.Contains(t, runs, "sidx_run_id.json")
	require.Contains(t, defs, "sidx_id.json")
	require.NotContains(t, defs, "sidx_run_id.json")
	require.NotContains(t, runs, "sidx_id.json")
}

// Static guard: production code reaches the embedded database through
// internal/wardendb (shared mode) or one of the schema-1 compat constructors
// listed here. Daemon wiring and the storage root must never call scriva.Open themselves.
func TestNoIndependentScrivaOpenInProduction(t *testing.T) {
	// Schema-1 compat constructors, reachable only through openCompat / legacy
	// migration. Deleted with the schema-2 cutover; shrink, never grow.
	legacy := map[string]bool{
		"internal/agentstore/store.go":       true,
		"internal/autopilot/store.go":        true,
		"internal/autopilotstore/store.go":   true,
		"internal/autopilotstore/migrate.go": true,
		"internal/backendstore/store.go":     true,
		"internal/backendstore/integrity.go": true, // schema-1 backends-db repair
		"internal/ctxstore/ctxstore.go":      true,
		"internal/knownprompts/store.go":     true,
		"internal/mailbox/mailbox.go":        true,
		"internal/pipeline/store.go":         true,
		"internal/planexport/store.go":       true,
		"internal/planstore/store.go":        true,
		"internal/projectstore/store.go":     true,
		"internal/schedule/store.go":         true,
		"internal/snapshot/store.go":         true,
		"internal/store/file.go":             true,
		"internal/terminalstore/store.go":    true,
		// Offline tooling that works on collection directories, never on a
		// running daemon's handle.
		"internal/repair/repair.go":             true,
		"internal/updater/fixture_generator.go": true,
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if fi.IsDir() {
			switch fi.Name() {
			case ".git", ".worktrees", "node_modules", "site", "web", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		imports := map[string]bool{}
		for _, im := range f.Imports {
			v := strings.Trim(im.Path.Value, `"`)
			if v == "github.com/srjn45/scriva" || v == "github.com/srjn45/scriva/engine" {
				imports[v] = true
			}
		}
		opens := false
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "scriva" || x.Name == "engine") && strings.HasPrefix(sel.Sel.Name, "Open") {
					opens = true
				}
			}
			return true
		})
		if !opens {
			return nil
		}
		switch {
		case strings.HasPrefix(rel, "internal/wardendb/"):
		case legacy[rel]:
		default:
			offenders = append(offenders, rel)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders, "independent scriva.Open outside internal/wardendb and the schema-1 compat constructors")
}

// The daemon, storage root and cmd must not import the embedded database
// engine directly at all.
func TestDaemonWiringDoesNotImportEngine(t *testing.T) {
	for _, dir := range []string{"../daemon", "../storage", "../../cmd"} {
		fset := token.NewFileSet()
		err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			require.NoError(t, perr)
			for _, im := range f.Imports {
				require.False(t, strings.HasPrefix(strings.Trim(im.Path.Value, `"`), "github.com/srjn45/scriva"),
					"%s imports the embedded engine directly", p)
			}
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
	}
}

// A schema-1 ledger beside warden-db is two candidate authorities: Open must
// refuse rather than silently fall back to (and start) the legacy stores.
func TestOpenRefusesSchema1LedgerBesideWardenDB(t *testing.T) {
	d := t.TempDir()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1}))
	require.NoError(t, os.MkdirAll(wardendb.Dir(d), 0o700))
	_, err := Open(d)
	var ge *schema.GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, schema.VerdictSchema2Residue, ge.Verdict)
	_, statErr := os.Stat(filepath.Join(d, "agents-db"))
	require.True(t, os.IsNotExist(statErr), "legacy stores must not be created")
}
