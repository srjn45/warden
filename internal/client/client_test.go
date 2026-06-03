package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsConnRefused(t *testing.T) {
	require.True(t, isConnRefused(syscall.ECONNREFUSED))
	require.False(t, isConnRefused(errors.New("some other transport error")),
		"non-refused errors must not be reported as a down daemon")
}

func TestListSessions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/sessions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[{"id":"A-1","status":"working"}]}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	out, err := c.List(t.Context())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "A-1", out[0].ID)
}

func TestDaemonDownGivesFriendlyError(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listening
	_, err := c.List(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDaemonDown)
}

func TestSpawn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/spawn", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"A-1","status":"spawning"}`))
	}))
	defer ts.Close()
	s, err := New(ts.URL).Spawn(t.Context(), SpawnParams{Type: "development", Ticket: "A-1", Repo: "/repo"})
	require.NoError(t, err)
	require.Equal(t, "A-1", s.ID)
}

func TestListDirs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/fs/dirs", r.URL.Path)
		require.Equal(t, "/home/me/work", r.URL.Query().Get("path"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"/home/me/work","parent":"/home/me","entries":[{"name":"api","path":"/home/me/work/api"}]}`))
	}))
	defer ts.Close()

	l, err := New(ts.URL).ListDirs(t.Context(), "/home/me/work")
	require.NoError(t, err)
	require.Equal(t, "/home/me/work", l.Path)
	require.Equal(t, "/home/me", l.Parent)
	require.Len(t, l.Entries, 1)
	require.Equal(t, "api", l.Entries[0].Name)
	require.Equal(t, "/home/me/work/api", l.Entries[0].Path)
}

func TestRemoveWorktreeConflictIsStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"uncommitted changes"}`))
	}))
	defer ts.Close()
	err := New(ts.URL).RemoveWorktree(t.Context(), "A-1", false)
	require.Error(t, err)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, 409, se.Code)
}
