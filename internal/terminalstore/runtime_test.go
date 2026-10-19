package terminalstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// LegacyTerminal simulates an old decoder without the Runtime field.
type LegacyTerminal struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	TmuxSession string    `json:"tmux_session,omitempty"`
	ProjectID   string    `json:"project_id,omitempty"`
	Workdir     string    `json:"workdir,omitempty"`
	Shell       string    `json:"shell,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Status      Status    `json:"status,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func findTerminalSegmentFile(t testing.TB, dir string) string {
	t.Helper()
	colDir := filepath.Join(dir, "terminals-db", "terminals")
	entries, err := os.ReadDir(colDir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ndjson") || strings.HasSuffix(e.Name(), ".jsonl") || strings.Contains(e.Name(), "segment") {
			return filepath.Join(colDir, e.Name())
		}
	}
	t.Fatalf("no segment file found in %s", colDir)
	return ""
}

func TestTerminalRoundTrip_LegacyRecordToNewBinary(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy_terminal.json"))
	require.NoError(t, err)

	var term Terminal
	require.NoError(t, json.Unmarshal(fixture, &term))
	require.Equal(t, "terminal-legacy-001", term.ID)
	require.Nil(t, term.Runtime, "legacy record decodes with Runtime == nil")

	// Round-trip marshal: omitempty ensures runtime is omitted
	out, err := json.Marshal(&term)
	require.NoError(t, err)
	require.NotContains(t, string(out), `"runtime"`)
}

func TestTerminalRoundTrip_NewRecordToOldDecoder(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "new_terminal.json"))
	require.NoError(t, err)

	// Decoded by new binary
	var term Terminal
	require.NoError(t, json.Unmarshal(fixture, &term))
	require.NotNil(t, term.Runtime)
	require.Equal(t, "direct", term.Runtime.Kind)
	require.Equal(t, "on", term.Runtime.PinnedMode)
	require.Equal(t, 12345, term.Runtime.HolderPID)
	require.Equal(t, 1, term.Runtime.Proto)

	// Decoded by old binary decoder: unknown fields are ignored
	var old LegacyTerminal
	require.NoError(t, json.Unmarshal(fixture, &old))
	require.Equal(t, "terminal-new-002", old.ID)
	require.Equal(t, "new-term", old.Name)
	require.Equal(t, "/tmp", old.Workdir)
}

func TestTerminalStore_MixedList(t *testing.T) {
	s, err := newTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	legacy := &Terminal{
		ID:          "terminal-leg",
		Name:        "legacy",
		TmuxSession: "terminal-leg",
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
		Runtime:     nil, // legacy
	}
	require.NoError(t, s.Insert(ctx, legacy))

	newTerm := &Terminal{
		ID:          "terminal-dir",
		Name:        "direct",
		TmuxSession: "terminal-dir",
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
		Runtime: &store.SessionRuntime{
			Kind:       "direct",
			PinnedMode: "on",
			LaunchedAt: &now,
		},
	}
	require.NoError(t, s.Insert(ctx, newTerm))

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)

	foundLeg := false
	foundNew := false
	for _, term := range list {
		if term.ID == "terminal-leg" {
			foundLeg = true
			require.Nil(t, term.Runtime, "legacy terminal retains nil runtime")
		} else if term.ID == "terminal-dir" {
			foundNew = true
			require.NotNil(t, term.Runtime, "new terminal retains runtime")
			require.Equal(t, "direct", term.Runtime.Kind)
			require.Equal(t, "on", term.Runtime.PinnedMode)
		}
	}
	require.True(t, foundLeg)
	require.True(t, foundNew)
}

func TestTerminalStore_NoRewriteOnRead(t *testing.T) {
	skipShared(t)
	dir := t.TempDir()
	s, err := newTestStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	term := &Terminal{
		ID:          "term-read-only",
		Name:        "read-only",
		TmuxSession: "term-read-only",
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, s.Insert(ctx, term))

	// Find the underlying segment file
	segPath := findTerminalSegmentFile(t, dir)
	statBefore, err := os.Stat(segPath)
	require.NoError(t, err)
	bytesBefore, err := os.ReadFile(segPath)
	require.NoError(t, err)

	// Perform multiple reads
	got, err := s.Get(ctx, term.ID)
	require.NoError(t, err)
	require.Equal(t, term.ID, got.ID)

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Verify no rewrite occurred
	statAfter, err := os.Stat(segPath)
	require.NoError(t, err)
	require.Equal(t, statBefore.ModTime(), statAfter.ModTime(), "file mod time unchanged after read")
	require.Equal(t, statBefore.Size(), statAfter.Size(), "file size unchanged after read")

	bytesAfter, err := os.ReadFile(segPath)
	require.NoError(t, err)
	require.Equal(t, bytesBefore, bytesAfter, "file content byte-for-byte identical after read")
}
