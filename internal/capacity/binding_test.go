package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type catalog map[string]backendstore.ModelEntry

func (c catalog) GetModel(backend, model string) (backendstore.ModelEntry, error) {
	v, ok := c[backend+":"+model]
	if !ok {
		return backendstore.ModelEntry{}, errors.New("missing")
	}
	return v, nil
}

func TestResolverBindsRouteAndKeepsProfilesIsolated(t *testing.T) {
	profiles := map[string]string{"claude": "account-a@example.test"}
	r := NewResolver(catalog{"claude:sonnet": {BackendID: "claude", ModelID: "sonnet", QuotaScope: "session"}}, func(_ context.Context, id string) (string, error) { return profiles[id], nil })
	a, err := r.Resolve(context.Background(), "claude", "sonnet")
	require.NoError(t, err)
	profiles["claude"] = "account-b@example.test"
	b, err := r.Resolve(context.Background(), "claude", "sonnet")
	require.NoError(t, err)
	require.Equal(t, []string{"session"}, a.MandatoryBuckets)
	require.Equal(t, "claude", a.Domain.Provider)
	require.Equal(t, "claude", a.Domain.AiCli)
	require.Equal(t, "sonnet", a.Domain.Route)
	require.NotEqual(t, a.Domain.AccountFingerprint, b.Domain.AccountFingerprint)
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "account-a@example.test")
	require.False(t, strings.Contains(string(raw), "token"))
}

func TestResolverUsesModelRouteScope(t *testing.T) {
	r := NewResolver(catalog{"antigravity:gemini": {QuotaScope: "gemini"}}, nil)
	b, err := r.Resolve(context.Background(), "antigravity", "gemini")
	require.NoError(t, err)
	require.Equal(t, []string{"gemini"}, b.Domain.BucketKeys)
	require.Equal(t, b.Domain.BucketKeys, b.MandatoryBuckets)
}
