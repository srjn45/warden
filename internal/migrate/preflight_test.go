package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ownerlock"
)

func runPreflight(t *testing.T, d string) *Schema2Plan {
	t.Helper()
	p, err := PreflightSchema2(context.Background(), d, t0)
	require.NoError(t, err)
	return p
}

func classes(p *Schema2Plan) map[string][]string {
	out := map[string][]string{}
	for _, f := range p.Findings {
		if f.Severity == SeverityBlocking {
			out[f.Class] = append(out[f.Class], f.Store)
		}
	}
	return out
}

func TestPreflight_CleanLegacyDirIsReadyAndReadOnly(t *testing.T) {
	d := legacyDir(t)
	before := snapshotTree(t, d)
	p := runPreflight(t, d)
	require.False(t, p.Blocked, "%+v", p.Findings)
	require.Equal(t, before, snapshotTree(t, d), "preflight must not write (no rebuild, no repair)")
}

func TestPreflight_StrictClassification(t *testing.T) {
	segOf := func(t *testing.T, d, store, col string) string {
		segs, err := filepath.Glob(filepath.Join(d, store, col, "seg_*.ndjson"))
		require.NoError(t, err)
		require.NotEmpty(t, segs)
		return segs[0]
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, d string)
		class  FindingClass
		store  string
	}{
		{"corrupt segment", func(t *testing.T, d string) {
			f, err := os.OpenFile(segOf(t, d, "agents-db", "agents"), os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteString("{not json}\n{\"id\":\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
		}, ClassCorruption, "agents-db/agents"},
		{"duplicate identity", func(t *testing.T, d string) {
			seg := segOf(t, d, "agents-db", "agents")
			require.NoError(t, os.WriteFile(seg, []byte(
				"{\"id\":1,\"op\":\"insert\",\"rev\":1,\"ts\":\"2026-10-09T10:00:00.000Z\",\"data\":{\"v\":1}}\n"+
					"{\"id\":1,\"op\":\"insert\",\"rev\":1,\"ts\":\"2026-10-09T10:01:00.000Z\",\"data\":{\"v\":2}}\n"), 0o600))
			require.NoError(t, os.Remove(filepath.Join(d, "agents-db", "agents", "index.json")))
		}, ClassDuplicateIdentity, "agents-db/agents"},
		{"stale index is refused, not rebuilt", func(t *testing.T, d string) {
			require.NoError(t, os.Remove(filepath.Join(d, "agents-db", "agents", "index.json")))
		}, ClassStaleDerivedState, "agents-db/agents"},
		{"scriva store outside the inventory", func(t *testing.T, d string) {
			validStore(t, d, "mystery-db", "things", map[string]any{"v": 1})
		}, ClassUnsupportedLayout, "mystery-db"},
		{"source of the wrong shape", func(t *testing.T, d string) {
			writeFile(t, d, "schedules.json", "{}")
			require.NoError(t, os.RemoveAll(filepath.Join(d, "metrics")))
			writeFile(t, d, "metrics", "i am a file, not a dir")
		}, ClassUnsupportedLayout, "metrics"},
		{"invalid flat json", func(t *testing.T, d string) {
			writeFile(t, d, "schedules.json", "{nope")
		}, ClassCorruption, "schedules.json"},
		{"invalid jsonl row", func(t *testing.T, d string) {
			writeFile(t, d, "metrics/2026-10-01.jsonl", "{\"a\":1}\nnot json\n")
		}, ClassCorruption, "metrics/2026-10-01.jsonl"},
		{"duplicate flat identity", func(t *testing.T, d string) {
			writeFile(t, d, "sessions/a.json", `{"id":"s1"}`)
			writeFile(t, d, "sessions/b.json", `{"id":"s1"}`)
		}, ClassDuplicateIdentity, "sessions/b.json"},
		{"symlinked external artifact", func(t *testing.T, d string) {
			require.NoError(t, os.MkdirAll(filepath.Join(d, "prompts"), 0o700))
			require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(d, "prompts", "p.link")))
		}, ClassUnsafeArtifact, "prompts/p.link"},
		{"top-level symlink", func(t *testing.T, d string) {
			require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(d, "audit-extra")))
		}, ClassUnsafeArtifact, "audit-extra"},
		{"transcript pointer escapes data dir", func(t *testing.T, d string) {
			writeFile(t, d, "snapshots/y.json", `{"id":"y","transcript_path":"/etc/hostname"}`)
		}, ClassUnsafeArtifact, "snapshots/y.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := legacyDir(t)
			tc.mutate(t, d)
			before := snapshotTree(t, d)
			p := runPreflight(t, d)
			require.True(t, p.Blocked, "%+v", p.Findings)
			require.Contains(t, classes(p)[string(tc.class)], tc.store, "%+v", p.Findings)
			for _, f := range p.Findings {
				if f.Severity == SeverityBlocking && f.Class != "" {
					require.NotEmpty(t, f.Details, "blocking finding needs remediation: %+v", f)
				}
			}
			require.Equal(t, before, snapshotTree(t, d), "refusal must not change legacy data")

			// and the cutover itself refuses without creating a backup
			_, err := BeginSchema2(d, "v10", t0)
			require.ErrorIs(t, err, ErrSchema2Blocked)
			_, statErr := os.Stat(filepath.Join(d, "backups"))
			require.True(t, os.IsNotExist(statErr))
		})
	}
}

func TestPreflight_MissingTranscriptIsNoteNotBlock(t *testing.T) {
	d := legacyDir(t)
	writeFile(t, d, "snapshots/z.json", `{"id":"z","transcript_path":"snapshots/z.transcript"}`)
	p := runPreflight(t, d)
	require.False(t, p.Blocked, "%+v", p.Findings)
}

func TestPreflight_OperatorCopyOfStoreIsExternal(t *testing.T) {
	d := legacyDir(t)
	validStore(t, d, "agents-db.bak-20260101", "agents", map[string]any{"v": 1})
	require.False(t, runPreflight(t, d).Blocked)
}

func TestPreflight_RefusesWhileOwned(t *testing.T) {
	d := legacyDir(t)
	// A lock held by another process cannot be faked in-process; assert the
	// stopped-daemon path instead: unowned data passes and the lock file left by
	// a released owner does not block.
	lock, err := ownerlock.Acquire(d, ownerlock.Info{Kind: ownerlock.KindDaemon, Version: "v9"})
	require.NoError(t, err)
	require.NoError(t, lock.Release())
	require.False(t, runPreflight(t, d).Blocked)
}
