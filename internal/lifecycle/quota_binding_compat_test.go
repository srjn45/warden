package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/stretchr/testify/require"
)

// TestHotSwapAcquiresQuotaBindingForUnboundLegacy is the Phase 10 migration
// rule: an older agent that only has backend/model fields stays operable as
// unbound_legacy, then acquires a daemon-owned QuotaBinding on the next safe
// lifecycle transition (HotSwap) — never by guessing account/bucket from thin air.
func TestHotSwapAcquiresQuotaBindingForUnboundLegacy(t *testing.T) {
	lc, _, sess := newSwapLC(t)
	writeClaudeTranscript(t, lc, sess)
	require.Nil(t, sess.QuotaBinding, "legacy fixture must start unbound")
	require.Equal(t, capacity.LegacyUnbound, sess.CapacityBindingState())

	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	lc.CapacityResolver = capacity.NewResolver(bs, func(_ context.Context, aiCli string) (string, error) {
		require.Equal(t, "codex", aiCli)
		return "operator-profile-a", nil
	})

	_, err = lc.HotSwap(context.Background(), sess, SwapRequest{
		Backend: "codex", Model: "gpt-5-codex", Reason: SwapReasonManual,
	})
	require.NoError(t, err)
	require.NotNil(t, sess.QuotaBinding, "HotSwap must persist a daemon-owned binding")
	require.Equal(t, "codex", sess.QuotaBinding.Domain.Provider)
	require.Equal(t, "gpt-5-codex", sess.QuotaBinding.Domain.Route)
	require.NotEmpty(t, sess.QuotaBinding.Domain.AccountFingerprint)
	require.True(t, sess.QuotaBinding.RequiresBucket(backendstore.DefaultQuotaScope) ||
		len(sess.QuotaBinding.MandatoryBuckets) > 0)
	require.Equal(t, "bound", sess.CapacityBindingState())
	require.NotContains(t, sess.QuotaBinding.Domain.AccountFingerprint, "operator-profile-a",
		"raw profile labels must never land in the persisted fingerprint")
}

// TestRestoreLeavesUnboundLegacyWithoutGuessing proves Restore keeps a legacy
// unbound record operable without inventing a quota binding — account/profile
// reconstruction on resume is not safe, so bulk recovery stays skipped until a
// later HotSwap (or fresh spawn) writes a real binding.
func TestRestoreLeavesUnboundLegacyWithoutGuessing(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	sid := "77777777-7777-4777-8777-777777777777"
	pdir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pdir, sid+".jsonl"), []byte("{}"), 0o644))

	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t agent-legacy-r": {Err: errStub("no session")},
	}}
	lc := New(fr, &FakeConfig{})
	lc.ProjectsDir = root

	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	lc.CapacityResolver = capacity.NewResolver(bs, func(_ context.Context, _ string) (string, error) {
		return "should-not-be-used-for-unbound", nil
	})

	sess := &agentstore.Agent{
		ID: "agent-legacy-r", TmuxSession: "agent-legacy-r",
		AiCli: "claude", Model: "sonnet", Workdir: workdir, AICLISessionID: sid,
		// QuotaBinding intentionally nil — backend/model only.
	}
	require.Equal(t, capacity.LegacyUnbound, sess.CapacityBindingState())

	require.NoError(t, lc.Restore(context.Background(), sess))
	require.Nil(t, sess.QuotaBinding, "Restore must not guess a binding for unbound_legacy")
	require.Equal(t, capacity.LegacyUnbound, sess.CapacityBindingState())
}
