package plugin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestRequestRoundTrip(t *testing.T) {
	req := Request{
		ProtocolVersion: ProtocolVersion,
		Event:           EventPostCommit,
		Session:         SessionMeta{ID: "dev-abc", Type: "development", Repo: "/r", Worktree: ".worktrees/dev-abc", Branch: "dev-abc", Workdir: "/r/.worktrees/dev-abc"},
		Payload:         map[string]string{"sha": "deadbeef", "branch": "dev-abc"},
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)

	var got Request
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, req, got)
}

func TestResponseRoundTrip(t *testing.T) {
	resp := Response{ProtocolVersion: ProtocolVersion, OK: true, Message: "did the thing"}
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	var got Response
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, resp, got)
}

func TestResponseOmitsEmptyMessage(t *testing.T) {
	b, err := json.Marshal(Response{ProtocolVersion: 1, OK: true})
	require.NoError(t, err)
	require.NotContains(t, string(b), "message")
}

func TestMetaFromSession(t *testing.T) {
	s := &store.Session{ID: "x", Type: store.TypeDevelopment, Repo: "/r", Worktree: "w", Branch: "b", Workdir: "/r/w"}
	m := MetaFromSession(s)
	require.Equal(t, "x", m.ID)
	require.Equal(t, "development", m.Type)
	require.Equal(t, "/r", m.Repo)
	require.Equal(t, "w", m.Worktree)
	require.Equal(t, "b", m.Branch)
	require.Equal(t, "/r/w", m.Workdir)
}

func TestMetaFromNilSession(t *testing.T) {
	require.Equal(t, SessionMeta{}, MetaFromSession(nil))
}

func TestValidEvent(t *testing.T) {
	require.True(t, ValidEvent(EventPreSpawn))
	require.True(t, ValidEvent(EventPreTeardown))
	require.False(t, ValidEvent(HookEvent("bogus")))
	require.Len(t, AllEvents, 7)
}
