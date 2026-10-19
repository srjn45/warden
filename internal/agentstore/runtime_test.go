package agentstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// LegacyAgent simulates a decoder from an old binary that does not know about the Runtime field.
type LegacyAgent struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	TmuxSession string       `json:"tmux_session"`
	Prompt      string       `json:"prompt"`
	Subject     string       `json:"subject"`
	Status      store.Status `json:"status"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func TestAgentRoundTrip_LegacyRecordToNewBinary(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy_agent.json"))
	require.NoError(t, err)

	var ag Agent
	require.NoError(t, json.Unmarshal(fixture, &ag))
	require.Equal(t, "agent-legacy-001", ag.ID)
	require.Nil(t, ag.Runtime, "legacy record decodes with Runtime == nil")

	// Round-trip marshal: omitempty ensures runtime is omitted
	out, err := json.Marshal(&ag)
	require.NoError(t, err)
	require.NotContains(t, string(out), `"runtime"`)
}

func TestAgentRoundTrip_NewRecordToOldDecoder(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "new_agent.json"))
	require.NoError(t, err)

	// Decoded by new binary
	var ag Agent
	require.NoError(t, json.Unmarshal(fixture, &ag))
	require.NotNil(t, ag.Runtime)
	require.Equal(t, "tmux", ag.Runtime.Kind)
	require.Equal(t, "shadow", ag.Runtime.PinnedMode)

	// Decoded by old binary decoder: unknown fields are ignored
	var old LegacyAgent
	require.NoError(t, json.Unmarshal(fixture, &old))
	require.Equal(t, "agent-new-002", old.ID)
	require.Equal(t, "new-agent", old.Name)
	require.Equal(t, "implement feature", old.Prompt)
}

func TestAgentStore_MixedList(t *testing.T) {
	s, err := newTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	legacy := &Agent{
		ID:          "agent-leg",
		Name:        "legacy",
		TmuxSession: "agent-leg",
		Status:      store.StatusWorking,
		CreatedAt:   now,
		UpdatedAt:   now,
		Runtime:     nil, // legacy
	}
	require.NoError(t, s.Insert(ctx, legacy))

	newAg := &Agent{
		ID:          "agent-dir",
		Name:        "direct",
		TmuxSession: "agent-dir",
		Status:      store.StatusWorking,
		CreatedAt:   now,
		UpdatedAt:   now,
		Runtime: &store.SessionRuntime{
			Kind:       "direct",
			PinnedMode: "on",
			LaunchedAt: &now,
		},
	}
	require.NoError(t, s.Insert(ctx, newAg))

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)

	foundLeg := false
	foundNew := false
	for _, a := range list {
		if a.ID == "agent-leg" {
			foundLeg = true
			require.Nil(t, a.Runtime, "legacy agent retains nil runtime")
		} else if a.ID == "agent-dir" {
			foundNew = true
			require.NotNil(t, a.Runtime, "new agent retains runtime")
			require.Equal(t, "direct", a.Runtime.Kind)
			require.Equal(t, "on", a.Runtime.PinnedMode)
		}
	}
	require.True(t, foundLeg)
	require.True(t, foundNew)
}

func TestAgentStore_NoRewriteOnRead(t *testing.T) {
	skipShared(t)
	s, err := newTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	ag := &Agent{
		ID:          "agent-read-only",
		Name:        "read-only",
		TmuxSession: "agent-read-only",
		Status:      store.StatusWorking,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, s.Insert(ctx, ag))

	// Find the underlying segment file
	segPath := findSegmentFile(t, s, "agents")
	statBefore, err := os.Stat(segPath)
	require.NoError(t, err)
	bytesBefore, err := os.ReadFile(segPath)
	require.NoError(t, err)

	// Perform multiple reads
	got, err := s.Get(ctx, ag.ID)
	require.NoError(t, err)
	require.Equal(t, ag.ID, got.ID)

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
