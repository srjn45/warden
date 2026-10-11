package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestSharedCollectionConfigParity(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)

	cfg := s.col.Config()
	require.Equal(t, engine.SyncModeAlways, cfg.SyncMode)
	require.Zero(t, cfg.MaxRecords)
	require.Zero(t, cfg.MaxBytes)

	infos, err := db.Registered()
	require.NoError(t, err)
	require.Len(t, infos, len(CollectionSpecs()))
	for i, want := range CollectionSpecs() {
		require.Equal(t, want.Name, infos[i].Name)
		require.Equal(t, want.Version, infos[i].Version)
	}

	_, err = NewStoreFromDB(db)
	require.ErrorIs(t, err, wardendb.ErrDuplicateCollection)
}

func TestSharedStoreDoesNotCloseHandle(t *testing.T) {
	db, err := wardendb.Open(t.TempDir())
	require.NoError(t, err)
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Put(&Snapshot{ID: "snap-x"}, ""))
	require.NoError(t, db.Close())
}

func TestSharedTranscriptStaysExternalWithRef(t *testing.T) {
	dataDir := t.TempDir()
	db, err := wardendb.Open(dataDir)
	require.NoError(t, err)
	defer db.Close()
	s, err := NewStoreFromDB(db)
	require.NoError(t, err)

	body := strings.Repeat("scrollback\n", 100)
	snap := &Snapshot{ID: "snap-a"}
	require.NoError(t, s.Put(snap, body))

	got, err := s.Get("snap-a")
	require.NoError(t, err)
	require.Len(t, got.Artifacts, 1)
	ref := got.Artifacts[0]
	require.Equal(t, ArtifactTranscript, ref.Kind)
	require.Equal(t, "snapshots/snap-a.transcript", ref.Path)
	require.EqualValues(t, len(body), ref.Size)
	require.Len(t, ref.SHA256, 64)
	blob, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(ref.Path)))
	require.NoError(t, err)
	require.Equal(t, body, string(blob))

	fs, err := s.Reconcile("snap-a")
	require.NoError(t, err)
	require.Len(t, fs, 1)
	require.True(t, fs[0].OK(), fs[0])
}

func TestReconcileArtifact(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.log"), []byte("hello"), 0o600))
	good := ArtifactRef{Kind: ArtifactLog, Path: "a.log", Size: 5}

	require.Equal(t, ArtifactOK, ReconcileArtifact(dir, good).Status)
	require.Equal(t, ArtifactMissing, ReconcileArtifact(dir, ArtifactRef{Path: "nope.log", Size: 1}).Status)

	bad := good
	bad.Size = 9
	f := ReconcileArtifact(dir, bad)
	require.Equal(t, ArtifactSizeMismatch, f.Status)
	require.EqualValues(t, 5, f.ActualSize)

	bad = good
	bad.SHA256 = strings.Repeat("0", 64)
	require.Equal(t, ArtifactHashMismatch, ReconcileArtifact(dir, bad).Status)

	for _, p := range []string{"", "../x", "/etc/passwd", ".."} {
		require.Equal(t, ArtifactUnsafePath, ReconcileArtifact(dir, ArtifactRef{Path: p}).Status, p)
	}

	// Reconcile never deletes: the mismatching file is still there.
	_, err := os.Stat(filepath.Join(dir, "a.log"))
	require.NoError(t, err)
}

func TestReconcileReportsMissingTranscriptAndKeepsRecord(t *testing.T) {
	st := newTestStore(t)
	require.NoError(t, st.Put(&Snapshot{ID: "snap-m"}, "data\n"))
	require.NoError(t, os.Remove(st.transcriptPath("snap-m")))
	fs, err := st.Reconcile("snap-m")
	require.NoError(t, err)
	require.Len(t, fs, 1)
	require.Equal(t, ArtifactMissing, fs[0].Status)
	_, err = st.Get("snap-m")
	require.NoError(t, err)
}

func TestSchema1RecordWithoutArtifactsDecodes(t *testing.T) {
	st := newTestStore(t)
	old := map[string]any{"id": "snap-old", "workdir": "/w", "created_at": "2026-01-01T00:00:00Z", "transcript_path": "/nowhere"}
	b, _ := json.Marshal(old)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	_, err := st.col.Upsert("snap-old", m)
	require.NoError(t, err)
	got, err := st.Get("snap-old")
	require.NoError(t, err)
	require.Empty(t, got.Artifacts)
	fs, err := st.Reconcile("snap-old")
	require.NoError(t, err)
	require.Equal(t, ArtifactMissing, fs[0].Status)
}
