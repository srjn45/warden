package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func TestSwitchExplicitBackend(t *testing.T) {
	_ = withTestBackendStore(t)

	var recordedID string
	var recordedParams client.SwitchSessionParams
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, id string, params client.SwitchSessionParams) (*lifecycle.SwapResult, error) {
		recordedID = id
		recordedParams = params
		return &lifecycle.SwapResult{
			Session:      &store.Session{ID: id},
			FromBackend:  "claude",
			FromModel:    "opus",
			ToBackend:    params.Backend,
			ToModel:      params.Model,
			HandoffPath:  "/tmp/handoff-agent-1.md",
			Reason:       lifecycle.SwapReason(params.Reason),
			ResolverUsed: false,
			Handoff:      handoff.Handoff{Goal: "Do work"},
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, "127.0.0.1:0", "switch", "agent-1", "--backend", "codex", "--model", "gpt-5-codex")
	require.NoError(t, err)
	require.Contains(t, out, "switched agent agent-1: claude (opus) → codex (gpt-5-codex)")
	require.Contains(t, out, "handoff written to /tmp/handoff-agent-1.md")
	require.Equal(t, "agent-1", recordedID)
	require.Equal(t, "codex", recordedParams.Backend)
	require.Equal(t, "gpt-5-codex", recordedParams.Model)
}

func TestSwitchByTier(t *testing.T) {
	_ = withTestBackendStore(t)

	var recordedParams client.SwitchSessionParams
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, id string, params client.SwitchSessionParams) (*lifecycle.SwapResult, error) {
		recordedParams = params
		return &lifecycle.SwapResult{
			Session:      &store.Session{ID: id},
			FromBackend:  "claude",
			FromModel:    "opus",
			ToBackend:    "antigravity",
			ToModel:      "gemini-3.1-pro",
			HandoffPath:  "/tmp/handoff-agent-2.md",
			Reason:       lifecycle.SwapReason(params.Reason),
			ResolverUsed: true,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, "127.0.0.1:0", "switch", "agent-2", "--tier", "tier-2", "--prompt", "Run tests", "--reason", "quota")
	require.NoError(t, err)
	require.Contains(t, out, "switched agent agent-2: claude (opus) → antigravity (gemini-3.1-pro)")
	require.Equal(t, "tier-2", recordedParams.Tier)
	require.Equal(t, "Run tests", recordedParams.Prompt)
	require.Equal(t, "quota", recordedParams.Reason)
}

func TestSwitchJSON(t *testing.T) {
	_ = withTestBackendStore(t)

	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, id string, params client.SwitchSessionParams) (*lifecycle.SwapResult, error) {
		return &lifecycle.SwapResult{
			Session:      &store.Session{ID: id},
			FromBackend:  "claude",
			ToBackend:    "codex",
			HandoffPath:  "/tmp/handoff-agent-3.md",
			Reason:       lifecycle.SwapReasonManual,
			ResolverUsed: false,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, "127.0.0.1:0", "switch", "agent-3", "--backend", "codex", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"from_backend": "claude"`)
	require.Contains(t, out, `"to_backend": "codex"`)
	require.Contains(t, out, `"handoff_path": "/tmp/handoff-agent-3.md"`)
}

func TestSwitchFromSessionEnv(t *testing.T) {
	_ = withTestBackendStore(t)

	t.Setenv("WARDEN_SESSION_ID", "self-agent")

	var invoked bool
	origHotSwap := runHotSwap
	runHotSwap = func(ctx context.Context, cmd *cobra.Command, id string, params client.SwitchSessionParams) (*lifecycle.SwapResult, error) {
		invoked = true
		require.Equal(t, "self-agent", id)
		return &lifecycle.SwapResult{
			Session:     &store.Session{ID: id},
			FromBackend: "claude",
			ToBackend:   "antigravity",
			HandoffPath: "/tmp/handoff-self.md",
			Reason:      lifecycle.SwapReasonManual,
		}, nil
	}
	t.Cleanup(func() { runHotSwap = origHotSwap })

	out, err := runGit(t, "127.0.0.1:0", "switch", "--backend", "antigravity")
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
	_, err := runGit(t, "127.0.0.1:0", "switch", "a1", "--tier", "tier-bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}

// TestSwitchCallsDaemonEndpoint verifies the real (unstubbed) command posts to
// the daemon's switch endpoint with the flag values, rather than opening the
// live store directly. This is the single-owner enforcement: no FileStore is
// constructed by the CLI.
func TestSwitchCallsDaemonEndpoint(t *testing.T) {
	_ = withTestBackendStore(t)

	var gotPath, gotMethod string
	var gotBody client.SwitchSessionParams
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		res := lifecycle.SwapResult{
			Session:     &store.Session{ID: "agent-9"},
			FromBackend: "claude",
			FromModel:   "opus",
			ToBackend:   "codex",
			ToModel:     "gpt-5-codex",
			HandoffPath: "/tmp/handoff-agent-9.md",
			Reason:      lifecycle.SwapReasonManual,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	out, err := runGit(t, addr, "switch", "agent-9", "--backend", "codex", "--model", "gpt-5-codex", "--reason", "quota")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/api/v1/sessions/agent-9/switch", gotPath)
	require.Equal(t, "codex", gotBody.Backend)
	require.Equal(t, "gpt-5-codex", gotBody.Model)
	require.Equal(t, "quota", gotBody.Reason)
	require.Contains(t, out, "switched agent agent-9: claude (opus) → codex (gpt-5-codex)")
}

// TestSwitchDaemonUnavailable asserts that when the daemon is unreachable the
// command fails with a clear connection error and NEVER falls back to opening
// the live session store — no sessions-db (or its lock file) is created in the
// data dir.
func TestSwitchDaemonUnavailable(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("data_dir: "+dataDir+"\n"), 0o600))

	root := newRootCmd()
	root.SetArgs([]string{"switch", "agent-x", "--backend", "codex", "--addr", "127.0.0.1:1", "--config", cfgPath})
	err := root.Execute()
	require.Error(t, err)

	// The CLI must not have created a live store in the data dir.
	_, statErr := os.Stat(filepath.Join(dataDir, "sessions-db"))
	require.True(t, os.IsNotExist(statErr), "switch must not open the live store when the daemon is unavailable")
}
