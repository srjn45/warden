package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/store"
	"github.com/srjn45/warden/internal/schema"
)

// GenerateUpgradeFixtures writes deterministic golden upgrade fixture trees into baseDir:
// 1. v9.25.0-legacy: unversioned legacy install with sentinel files and ScrivaDB stores.
// 2. v9.26.0-waypoint: waypoint release with schema 1 ledger and clean stores.
// 3. v9.27.0-bad-history: schema 1 release carrying known bad-history shapes (revision regressions, duplicate keys, stray body id != key).
func GenerateUpgradeFixtures(baseDir string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}

	if err := generateLegacyFixture(filepath.Join(baseDir, "v9.25.0-legacy")); err != nil {
		return fmt.Errorf("generate legacy fixture: %w", err)
	}

	if err := generateWaypointFixture(filepath.Join(baseDir, "v9.26.0-waypoint")); err != nil {
		return fmt.Errorf("generate waypoint fixture: %w", err)
	}

	if err := generateBadHistoryFixture(filepath.Join(baseDir, "v9.27.0-bad-history")); err != nil {
		return fmt.Errorf("generate bad history fixture: %w", err)
	}

	return nil
}

func generateLegacyFixture(dir string) error {
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Legacy sentinel files from schema.LegacySentinels
	for _, s := range schema.LegacySentinels[:3] {
		p := filepath.Join(dir, filepath.FromSlash(s))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
			return err
		}
	}

	// Create valid stores: context, backends, agents-db
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := writeCollection(dir, "context", "context", []recordSpec{
		{Key: "ctx-1", ID: "ctx-1", Rev: 1, Ts: t0, Data: map[string]any{"_key": "ctx-1", "id": "ctx-1", "title": "System Context", "content": "Initial context"}},
	}); err != nil {
		return err
	}

	if err := writeCollection(dir, "backends", "backends", []recordSpec{
		{Key: "b-1", ID: "b-1", Rev: 1, Ts: t0, Data: map[string]any{"_key": "b-1", "id": "b-1", "name": "claude", "tier": "default", "enabled": true}},
	}); err != nil {
		return err
	}

	if err := writeCollection(dir, "agents-db", "agents", []recordSpec{
		{Key: "ag-1", ID: "ag-1", Rev: 1, Ts: t0, Data: map[string]any{"_key": "ag-1", "id": "ag-1", "name": "worker-1", "status": "idle"}},
	}); err != nil {
		return err
	}

	return nil
}

func generateWaypointFixture(dir string) error {
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "9.26.0",
		History: []schema.HistoryEntry{
			{From: 0, To: 1, Migration: "baseline-legacy", At: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)},
		},
	}
	if err := schema.Save(dir, l); err != nil {
		return err
	}

	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if err := writeCollection(dir, "context", "context", []recordSpec{
		{Key: "ctx-1", ID: "ctx-1", Rev: 1, Ts: t0, Data: map[string]any{"_key": "ctx-1", "id": "ctx-1", "title": "Waypoint Context", "content": "Updated"}},
		{Key: "ctx-2", ID: "ctx-2", Rev: 1, Ts: t0, Data: map[string]any{"_key": "ctx-2", "id": "ctx-2", "title": "Second Doc", "content": "Data"}},
	}); err != nil {
		return err
	}

	if err := writeCollection(dir, "backends", "backends", []recordSpec{
		{Key: "b-1", ID: "b-1", Rev: 1, Ts: t0, Data: map[string]any{"_key": "b-1", "id": "b-1", "name": "claude", "tier": "default", "enabled": true}},
		{Key: "b-2", ID: "b-2", Rev: 1, Ts: t0, Data: map[string]any{"_key": "b-2", "id": "b-2", "name": "gemini", "tier": "fast", "enabled": true}},
	}); err != nil {
		return err
	}

	return nil
}

func generateBadHistoryFixture(dir string) error {
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	l := &schema.Ledger{
		SchemaVersion: 1,
		BinaryVersion: "9.27.0",
		History: []schema.HistoryEntry{
			{From: 0, To: 1, Migration: "baseline-legacy", At: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)},
		},
	}
	if err := schema.Save(dir, l); err != nil {
		return err
	}

	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	// Write conflicting records matching the v9.25 -> v9.28 incident:
	// 1. Revision regression: rev 3 then rev 2 on same key
	// 2. Duplicate live key on separate record ID
	// 3. Stray record where body id != key
	colDir := filepath.Join(dir, "context", "context")
	if err := os.MkdirAll(colDir, 0o755); err != nil {
		return err
	}
	segPath := filepath.Join(colDir, "seg_000001.ndjson")
	f, err := os.Create(segPath)
	if err != nil {
		return err
	}
	defer f.Close()

	entries := []store.Entry{
		// rec 1 rev 1
		store.NewInsert(1, map[string]any{"_key": "ctx-1", "id": "ctx-1", "val": "v1"}),
		// rec 1 rev 3 (newer)
		{ID: 1, Op: store.OpUpdate, Rev: 3, Ts: t1, Data: map[string]any{"_key": "ctx-1", "id": "ctx-1", "val": "v3"}},
		// rec 1 rev 2 (regressed)
		{ID: 1, Op: store.OpUpdate, Rev: 2, Ts: t2, Data: map[string]any{"_key": "ctx-1", "id": "ctx-1", "val": "v2-regression"}},
		// rec 2 duplicate key ctx-1
		{ID: 2, Op: store.OpInsert, Rev: 1, Ts: t2, Data: map[string]any{"_key": "ctx-1", "id": "ctx-1", "val": "duplicate"}},
		// rec 3 stray record
		{ID: 3, Op: store.OpInsert, Rev: 1, Ts: t2, Data: map[string]any{"_key": "stray-key", "id": "different-body-id", "val": "stray"}},
	}

	for _, e := range entries {
		enc, err := store.Encode(e)
		if err != nil {
			return err
		}
		if _, err := f.Write(enc); err != nil {
			return err
		}
	}
	_ = f.Sync()

	metaPath := filepath.Join(colDir, "meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"name":"context"}`+"\n"), 0o644); err != nil {
		return err
	}

	return nil
}

type recordSpec struct {
	Key  string
	ID   string
	Rev  uint64
	Ts   time.Time
	Data map[string]any
}

func writeCollection(dataDir, storeName, collName string, specs []recordSpec) error {
	storeDir := filepath.Join(dataDir, storeName)
	collDir := filepath.Join(storeDir, collName)
	if err := os.MkdirAll(collDir, 0o755); err != nil {
		return err
	}

	segPath := filepath.Join(collDir, "seg_000001.ndjson")
	f, err := os.Create(segPath)
	if err != nil {
		return err
	}

	for i, sp := range specs {
		id := uint64(i + 1)
		sp.Data["_key"] = sp.Key
		entry := store.NewInsert(id, sp.Data)
		if sp.Rev > 0 {
			entry.Rev = sp.Rev
		}
		if !sp.Ts.IsZero() {
			entry.Ts = sp.Ts
		}
		enc, err := store.Encode(entry)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Write(enc); err != nil {
			_ = f.Close()
			return err
		}
	}
	_ = f.Close()

	metaPath := filepath.Join(collDir, "meta.json")
	if err := os.WriteFile(metaPath, []byte(fmt.Sprintf(`{"name":"%s"}`+"\n", collName)), 0o644); err != nil {
		return err
	}

	col, err := engine.OpenCollection(collName, storeDir, engine.CollectionConfig{})
	if err != nil {
		return err
	}
	_ = col.EnsureUniqueIndex("_key")
	return col.Close()
}
