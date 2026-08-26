package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/srjn45/warden/internal/agentbackend/backends"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func setupTestDaemonHotSwap(t *testing.T) (*backendstore.Store, *store.FileStore, *lifecycle.Lifecycle, *lifecycle.FakeRunner, *store.Session) {
	t.Helper()
	dataDir := t.TempDir()
	bs, err := backendstore.NewStore(filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	t.Cleanup(func() { bs.Close() })

	st, err := store.NewFileStore(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(context.Background()) })

	workdir := t.TempDir()
	sess := &store.Session{
		ID:          "agent-hs-1",
		TmuxSession: "agent-hs-1",
		Backend:     "claude",
		Model:       "opus",
		Role:        "implementation",
		Repo:        workdir,
		Workdir:     workdir,
		Branch:      "feat/hotswap",
		Worktree:    ".worktrees/agent-hs-1",
		Status:      store.StatusWorking,
	}
	require.NoError(t, st.Insert(context.Background(), sess))

	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	lc := lifecycle.New(fr, &lifecycle.FakeConfig{})
	lc.ProjectsDir = t.TempDir()
	lc.PromptsDir = filepath.Join(dataDir, "prompts")
	require.NoError(t, os.MkdirAll(lc.PromptsDir, 0o755))

	// Register installed backends so resolver finds eligible candidates
	require.NoError(t, bs.Upsert(backendstore.Backend{
		ID:        "antigravity",
		Installed: true,
		Enabled:   true,
		Tier:      backendstore.TierSubscription,
	}))
	require.NoError(t, bs.Upsert(backendstore.Backend{
		ID:        "claude",
		Installed: true,
		Enabled:   true,
		Tier:      backendstore.TierSubscription,
	}))

	// Wire resolver
	lc.Resolver = router.NewResolver(bs)

	return bs, st, lc, fr, sess
}

func TestDaemonPollerHotSwapWiring(t *testing.T) {
	bs, st, lc, _, sess := setupTestDaemonHotSwap(t)

	// Ensure handover settings are enabled
	require.NoError(t, bs.SetHandoverSettings(backendstore.HandoverSettings{
		Enabled:               true,
		ContextFillThreshold:  90,
		RollingQuotaThreshold: 90,
		CooldownPeriod:        15 * time.Minute,
	}))

	// Verify Resolver is set
	require.NotNil(t, lc.Resolver)

	// Build the OnHotSwap handler as wired in daemon
	var swapCompleted bool
	critTokensLimit := 200000
	onHotSwap := func(s *store.Session, tokens int) {
		settings, err := bs.GetHandoverSettings()
		if err != nil {
			settings = backendstore.DefaultHandoverSettings()
		}
		if !settings.Enabled {
			return
		}
		in := lifecycle.ThresholdInput{
			Settings:      settings,
			ContextTokens: tokens,
			ContextLimit:  critTokensLimit,
			ContextKnown:  tokens > 0 && critTokensLimit > 0,
		}
		if _, used, limit, _, qerr := bs.GetHeadroom(s.Backend, time.Now()); qerr == nil && limit > 0 {
			in.QuotaUsed = used
			in.QuotaLimit = limit
			in.QuotaKnown = true
		}
		sig := lifecycle.DecideHotSwap(in)
		if !sig.Trigger {
			return
		}
		swapReq := lifecycle.SwapRequest{
			Role:   s.Role,
			Reason: sig.Reason,
		}
		res, swapErr := lc.HotSwap(context.Background(), s, swapReq)
		if swapErr != nil {
			t.Fatalf("hot-swap failed: %v", swapErr)
		}
		_ = st.Update(context.Background(), s.ID, func(sess *store.Session) error {
			sess.Backend = s.Backend
			sess.Model = s.Model
			sess.ClaudeSessionID = s.ClaudeSessionID
			sess.UpdatedAt = s.UpdatedAt
			return nil
		})
		require.NotEmpty(t, res.ToBackend)
		swapCompleted = true
	}

	// Trigger with tokens that exceed threshold (190k / 200k = 95% >= 90%)
	onHotSwap(sess, 190000)

	require.True(t, swapCompleted, "hot swap should have triggered and completed")

	// Verify session was updated in store
	updated, err := st.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, updated.Backend)
}

func TestDaemonPollerHotSwapDisabled(t *testing.T) {
	bs, _, lc, _, sess := setupTestDaemonHotSwap(t)

	// Set handover settings to disabled
	require.NoError(t, bs.SetHandoverSettings(backendstore.HandoverSettings{
		Enabled: false,
	}))

	var swapCompleted bool
	critTokensLimit := 200000
	onHotSwap := func(s *store.Session, tokens int) {
		settings, err := bs.GetHandoverSettings()
		if err != nil {
			settings = backendstore.DefaultHandoverSettings()
		}
		if !settings.Enabled {
			return
		}
		in := lifecycle.ThresholdInput{
			Settings:      settings,
			ContextTokens: tokens,
			ContextLimit:  critTokensLimit,
			ContextKnown:  tokens > 0 && critTokensLimit > 0,
		}
		sig := lifecycle.DecideHotSwap(in)
		if !sig.Trigger {
			return
		}
		swapReq := lifecycle.SwapRequest{Role: s.Role, Reason: sig.Reason}
		_, _ = lc.HotSwap(context.Background(), s, swapReq)
		swapCompleted = true
	}

	onHotSwap(sess, 190000)
	require.False(t, swapCompleted, "disabled handover should not trigger swap")
}

func TestDaemonPollerHotSwapBelowThreshold(t *testing.T) {
	bs, _, lc, _, sess := setupTestDaemonHotSwap(t)

	require.NoError(t, bs.SetHandoverSettings(backendstore.HandoverSettings{
		Enabled:               true,
		ContextFillThreshold:  90,
		RollingQuotaThreshold: 90,
	}))

	var swapCompleted bool
	critTokensLimit := 200000
	onHotSwap := func(s *store.Session, tokens int) {
		settings, err := bs.GetHandoverSettings()
		if err != nil {
			settings = backendstore.DefaultHandoverSettings()
		}
		if !settings.Enabled {
			return
		}
		in := lifecycle.ThresholdInput{
			Settings:      settings,
			ContextTokens: tokens,
			ContextLimit:  critTokensLimit,
			ContextKnown:  tokens > 0 && critTokensLimit > 0,
		}
		sig := lifecycle.DecideHotSwap(in)
		if !sig.Trigger {
			return
		}
		swapReq := lifecycle.SwapRequest{Role: s.Role, Reason: sig.Reason}
		_, _ = lc.HotSwap(context.Background(), s, swapReq)
		swapCompleted = true
	}

	// 100k / 200k = 50% < 90%
	onHotSwap(sess, 100000)
	require.False(t, swapCompleted, "below threshold should not trigger swap")
}
