package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// switchSessionStub serves GET /sessions/<id> with a canned session record.
func switchSessionStub(t *testing.T, sess *store.Session) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess == nil {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sess)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestSwitchExplicitBackend(t *testing.T) {
	_ = withTestBackendStore(t)

	sess := &store.Session{
		ID:          "agent-1",
		TmuxSession: "agent-1",
		Backend:     "claude",
		Model:       "opus",
		Workdir:     t.TempDir(),
	}
	addr := switchSessionStub(t, sess)

	var recordedReq lifecycle.SwapRequest
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, s *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
		recordedReq = req
		return &lifecycle.SwapResult{
			Session:      s,
			FromBackend:  "claude",
			FromModel:    "opus",
			ToBackend:    req.Backend,
			ToModel:      req.Model,
			HandoffPath:  "/tmp/handoff-agent-1.md",
			Reason:       req.Reason,
			ResolverUsed: false,
			Handoff:      handoff.Handoff{Goal: "Do work"},
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, addr, "switch", "agent-1", "--backend", "codex", "--model", "gpt-5-codex")
	require.NoError(t, err)
	require.Contains(t, out, "switched agent agent-1: claude (opus) → codex (gpt-5-codex)")
	require.Contains(t, out, "handoff written to /tmp/handoff-agent-1.md")
	require.Equal(t, "codex", recordedReq.Backend)
	require.Equal(t, "gpt-5-codex", recordedReq.Model)
	require.Equal(t, lifecycle.SwapReasonManual, recordedReq.Reason)
}

func TestSwitchByTier(t *testing.T) {
	_ = withTestBackendStore(t)

	sess := &store.Session{
		ID:          "agent-2",
		TmuxSession: "agent-2",
		Backend:     "claude",
		Model:       "claude-opus",
		Workdir:     t.TempDir(),
	}
	addr := switchSessionStub(t, sess)

	var recordedReq lifecycle.SwapRequest
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, s *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
		recordedReq = req
		return &lifecycle.SwapResult{
			Session:      s,
			FromBackend:  "claude",
			FromModel:    "claude-opus",
			ToBackend:    "antigravity",
			ToModel:      "gemini-3.1-pro",
			HandoffPath:  "/tmp/handoff-agent-2.md",
			Reason:       req.Reason,
			ResolverUsed: true,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, addr, "switch", "agent-2", "--tier", "tier-2", "--prompt", "Run tests", "--reason", "quota")
	require.NoError(t, err)
	require.Contains(t, out, "switched agent agent-2: claude (claude-opus) → antigravity (gemini-3.1-pro)")
	require.Equal(t, backendstore.Tier2, recordedReq.Tier)
	require.Equal(t, "Run tests", recordedReq.Prompt)
	require.Equal(t, lifecycle.SwapReasonQuota, recordedReq.Reason)
}

func TestSwitchJSON(t *testing.T) {
	_ = withTestBackendStore(t)

	sess := &store.Session{
		ID:          "agent-3",
		TmuxSession: "agent-3",
		Backend:     "claude",
		Workdir:     t.TempDir(),
	}
	addr := switchSessionStub(t, sess)

	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, s *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
		return &lifecycle.SwapResult{
			Session:      s,
			FromBackend:  "claude",
			ToBackend:    "codex",
			HandoffPath:  "/tmp/handoff-agent-3.md",
			Reason:       lifecycle.SwapReasonManual,
			ResolverUsed: false,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, addr, "switch", "agent-3", "--backend", "codex", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"from_backend": "claude"`)
	require.Contains(t, out, `"to_backend": "codex"`)
	require.Contains(t, out, `"handoff_path": "/tmp/handoff-agent-3.md"`)
}

func TestSwitchFromSessionEnv(t *testing.T) {
	_ = withTestBackendStore(t)

	sess := &store.Session{
		ID:          "self-agent",
		TmuxSession: "self-agent",
		Backend:     "claude",
		Workdir:     t.TempDir(),
	}
	addr := switchSessionStub(t, sess)

	t.Setenv("WARDEN_SESSION_ID", "self-agent")

	var invoked bool
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, s *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
		invoked = true
		require.Equal(t, "self-agent", s.ID)
		return &lifecycle.SwapResult{
			Session:     s,
			FromBackend: "claude",
			ToBackend:   "antigravity",
			HandoffPath: "/tmp/handoff-self.md",
			Reason:      lifecycle.SwapReasonManual,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, addr, "switch", "--backend", "antigravity")
	require.NoError(t, err)
	require.True(t, invoked)
	require.Contains(t, out, "switched agent self-agent")
}

func TestSwitchNoAgentID(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")

	_, err := runGit(t, "127.0.0.1:0", "switch", "--backend", "codex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "switch requires an agent-id argument or must be run inside a warden agent session")
}

func TestSwitchInvalidTier(t *testing.T) {
	sess := &store.Session{ID: "a1", Workdir: t.TempDir()}
	addr := switchSessionStub(t, sess)

	_, err := runGit(t, addr, "switch", "a1", "--tier", "tier-bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}
