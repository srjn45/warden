package agentstore

import (
	"encoding/json"
	"testing"

	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCapacityBindingRoundTripsAcrossLegacySessionBoundary(t *testing.T) {
	a := &Agent{ID: "a1", AiCli: "claude", Model: "sonnet", QuotaBinding: &capacity.QuotaBinding{Domain: capacity.CapacityDomain{Provider: "claude", AiCli: "claude", AccountFingerprint: "sha256:opaque", Route: "sonnet", BucketKeys: []string{"session"}}, MandatoryBuckets: []string{"session"}}}
	s := a.ToSession()
	require.Equal(t, a.QuotaBinding, s.QuotaBinding)
	back := FromSession(s)
	require.Equal(t, a.QuotaBinding, back.QuotaBinding)
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	var decoded Agent
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, a.QuotaBinding, decoded.QuotaBinding)
}

func TestLegacySessionWithoutBindingStaysUnbound(t *testing.T) {
	var s store.Session
	require.NoError(t, json.Unmarshal([]byte(`{"id":"legacy","backend":"claude"}`), &s))
	a := FromSession(&s)
	require.Nil(t, a.QuotaBinding)
	require.Equal(t, capacity.LegacyUnbound, a.CapacityBindingState())
	require.Equal(t, capacity.LegacyUnbound, s.CapacityBindingState())
}
