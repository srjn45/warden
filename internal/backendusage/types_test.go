package backendusage

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLimitUnknownValuesAreExplicitNull(t *testing.T) {
	raw, err := json.Marshal(Limit{ID: "provider:pool", Scope: "pool", Label: "Pool"})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"provider:pool","scope":"pool","label":"Pool","model_families":null,"models":null,"used_percent":null,"resets_at":null}`, string(raw))
}

func TestScopedLimitsFixtureRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/scoped-limits.json")
	require.NoError(t, err)
	var snapshot Snapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.Len(t, snapshot.Backends[0].Usage, 4)
	require.Equal(t, "gemini", snapshot.Backends[0].Usage[0].Scope)
	require.Equal(t, "antigravity:gemini-5h", snapshot.Backends[0].Usage[0].ID)
	require.Nil(t, snapshot.Backends[0].Usage[2].UsedPercent)
	require.Nil(t, snapshot.Backends[0].Usage[2].ResetsAt)
}
