package agentstore

import (
	"encoding/json"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestExecutionProfileRoundTripsAcrossLegacySessionBoundary(t *testing.T) {
	a := &Agent{
		ID:               "a1",
		AiCli:            "cursor",
		Model:            "composer",
		ExecutionProfile: store.ExecutionProfile{Network: store.NetworkNone},
	}
	s := a.ToSession()
	require.Equal(t, a.ExecutionProfile, s.ExecutionProfile)
	back := FromSession(s)
	require.Equal(t, a.ExecutionProfile, back.ExecutionProfile)

	raw, err := json.Marshal(a)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"execution_profile"`)
	require.Contains(t, string(raw), `"network":"none"`)

	var decoded Agent
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, a.ExecutionProfile, decoded.ExecutionProfile)

	// Legacy JSON with no execution_profile → empty profile, not an error.
	var legacy Agent
	require.NoError(t, json.Unmarshal([]byte(`{"id":"legacy","ai_cli":"cursor"}`), &legacy))
	require.Equal(t, store.ExecutionProfile{}, legacy.ExecutionProfile)
	require.Equal(t, store.NetworkLoopback, legacy.ExecutionProfile.EffectiveNetwork())

	var legacySess store.Session
	require.NoError(t, json.Unmarshal([]byte(`{"id":"legacy","backend":"claude"}`), &legacySess))
	fromLegacy := FromSession(&legacySess)
	require.Equal(t, store.ExecutionProfile{}, fromLegacy.ExecutionProfile)
	require.Equal(t, store.NetworkLoopback, fromLegacy.ExecutionProfile.EffectiveNetwork())
}
