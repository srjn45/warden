package repair

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"
)

func TestRepairAll_ResolvesLiveWinsAndQuarantines(t *testing.T) {
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "context")
	collDir := filepath.Join(storeDir, "context")
	require.NoError(t, os.MkdirAll(collDir, 0o755))

	// Write conflicting records:
	// Record 1: key="k1", rev=1, title="version 1"
	// Record 2: key="k2", id="k2", rev=1, title="clean"
	// Record 3: key="k1", rev=2, title="version 2 (newer)"
	// Record 4: key="k1", rev=1, title="version 1 stale regression"
	// Record 5: key="stray-key", id="different-id", title="stray"
	seg1 := filepath.Join(collDir, "seg_000001.ndjson")
	lines := []string{
		`{"_key":"k1","_rev":1,"id":"k1","title":"version 1"}`,
		`{"_key":"k2","_rev":1,"id":"k2","title":"clean"}`,
		`{"_key":"k1","_rev":2,"id":"k1","title":"version 2 (newer)"}`,
		`{"_key":"k1","_rev":1,"id":"k1","title":"version 1 stale regression"}`,
		`{"_key":"stray-key","_rev":1,"id":"different-id","title":"stray"}`,
	}
	f, err := os.Create(seg1)
	require.NoError(t, err)
	for _, l := range lines {
		_, _ = f.WriteString(l + "\n")
	}
	require.NoError(t, f.Close())

	// Create meta.json
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "meta.json"), []byte(`{"name":"context"}`), 0o644))

	rep, err := RepairAll(context.Background(), Options{
		DataDir:        tmp,
		ResolveHistory: "live-wins",
		Version:        "9.29.0",
	})
	require.NoError(t, err)
	require.Empty(t, rep.ErrorMessage)
	require.True(t, rep.Clean)

	// Check results
	colls := rep.Stores["context"]
	require.Len(t, colls, 1)
	res := colls[0]
	require.Equal(t, "context", res.Name)
	require.Equal(t, 5, res.TotalRead)
	require.Equal(t, 2, res.KeptRecords)  // k1 and k2 kept
	require.Equal(t, 3, res.DroppedCount) // 2 duplicates for k1 + 1 stray

	// Verify quarantine dir exists
	require.DirExists(t, res.QuarantineDir)

	// Verify repaired collection can be opened cleanly by ScrivaDB engine
	col, err := engine.OpenCollection("context", storeDir, engine.CollectionConfig{})
	require.NoError(t, err)
	defer col.Close()

	recK1, err := col.GetByKey("k1")
	require.NoError(t, err)
	require.Equal(t, "version 1 stale regression", recK1.Data["title"])

	recK2, err := col.GetByKey("k2")
	require.NoError(t, err)
	require.Equal(t, "clean", recK2.Data["title"])

	_, err = col.GetByKey("stray-key")
	require.Error(t, err)
}

func TestRepairAll_DryRun(t *testing.T) {
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "backends")
	collDir := filepath.Join(storeDir, "backends")
	require.NoError(t, os.MkdirAll(collDir, 0o755))

	seg1 := filepath.Join(collDir, "seg_000001.ndjson")
	require.NoError(t, os.WriteFile(seg1, []byte(`{"_key":"b1","id":"b1"}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "meta.json"), []byte(`{"name":"backends"}`), 0o644))

	rep, err := RepairAll(context.Background(), Options{
		DataDir:        tmp,
		ResolveHistory: "live-wins",
		DryRun:         true,
		Version:        "9.29.0",
	})
	require.NoError(t, err)
	require.NotNil(t, rep)

	// In dry-run, no quarantine directory should be created
	colls := rep.Stores["backends"]
	require.Len(t, colls, 1)
	require.Empty(t, colls[0].QuarantineDir)
}
