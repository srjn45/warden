package ptyhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyholder"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
)

// fakeHolderLauncher implements the VerifyHolder interface for unit tests without launching a real holder daemon.
type fakeHolderLauncher struct {
	has       map[string]bool
	holderPID map[string]int
	start     map[string]uint64
	proto     map[string]int
}

func (f *fakeHolderLauncher) Launch(ctx context.Context, name string, argv []string, cwd string, env []string, cols, rows uint16) (*ptyruntime.Adoption, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeHolderLauncher) Adopt(ctx context.Context, name string) (*ptyruntime.Adoption, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeHolderLauncher) Has(name string) bool {
	return f.has[name]
}

func (f *fakeHolderLauncher) Release(ctx context.Context, name string) {
	delete(f.has, name)
}

func (f *fakeHolderLauncher) VerifyHolder(name string) (int, uint64, int, bool) {
	if !f.has[name] {
		return 0, 0, 0, false
	}
	return f.holderPID[name], f.start[name], f.proto[name], true
}

func TestResolve_Precedence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "pty")
	run := newTmux(t)
	m := New(Config{
		Mode:           ModeOn,
		Dir:            dir,
		Run:            run,
		Shell:          []string{"/bin/sh"},
		ReconcileEvery: 50 * time.Millisecond,
		Options:        ptyruntime.Options{IdleFlush: 20 * time.Millisecond, KillGrace: 500 * time.Millisecond, DrainGrace: 300 * time.Millisecond},
	})
	defer m.Close()

	records := map[string]*SessionRecord{}
	m.SetRecordResolver(func(_ context.Context, id string) (*SessionRecord, bool) {
		r, ok := records[id]
		return r, ok
	})

	// 1. Direct: live direct session in the manager
	t.Run("live direct terminal", func(t *testing.T) {
		const directID = "term-direct-live"
		records[directID] = &SessionRecord{
			ID:          directID,
			Kind:        "terminal",
			TmuxSession: directID,
			Runtime: &store.SessionRuntime{
				Kind:       "direct",
				PinnedMode: "on",
			},
		}
		require.NoError(t, m.TerminalHost().NewSession(ctx, directID, t.TempDir()))
		defer func() { _ = m.TerminalHost().KillSession(ctx, directID) }()

		info := m.Resolve(ctx, directID)
		require.Equal(t, "direct", info.Kind)
		require.Equal(t, "direct", info.Presentation)
		require.Equal(t, directID, info.Key)
		require.Equal(t, "live", info.Source)
		require.True(t, info.Authoritative)
		require.Contains(t, info.Capabilities, "native-attach")
		require.Contains(t, info.Capabilities, "input")
		require.Contains(t, info.Capabilities, "resize")
		require.Contains(t, info.Capabilities, "authoritative-state")
	})

	// 2. Holder-hosted after restart: no live direct session in memory, but launcher verifies holder
	t.Run("holder-hosted after restart", func(t *testing.T) {
		const holderID = "term-holder-restart"
		fakeLauncher := &fakeHolderLauncher{
			has:       map[string]bool{holderID: true},
			holderPID: map[string]int{holderID: os.Getpid()},
			start:     map[string]uint64{holderID: 12345},
			proto:     map[string]int{holderID: ptyholder.ProtoVersion},
		}
		m.direct.Launcher = fakeLauncher
		records[holderID] = &SessionRecord{
			ID:          holderID,
			Kind:        "terminal",
			TmuxSession: holderID,
			Runtime: &store.SessionRuntime{
				Kind:        "direct",
				PinnedMode:  "on",
				HolderPID:   os.Getpid(),
				HolderStart: 12345,
				Proto:       ptyholder.ProtoVersion,
			},
		}

		info := m.Resolve(ctx, holderID)
		require.Equal(t, "direct", info.Kind)
		require.Equal(t, "direct", info.Presentation)
		require.Equal(t, "holder", info.ProcessOwner)
		require.NotNil(t, info.Holder)
		require.Equal(t, os.Getpid(), info.Holder.PID)
		require.Equal(t, uint64(12345), info.Holder.StartTime)
		require.Equal(t, "live", info.Source)
		require.False(t, info.Authoritative, "unadopted holder-hosted session is not authoritative in memory yet")
		require.Contains(t, info.Capabilities, "survives-daemon")
	})

	// 3. Mirrored tmux session
	t.Run("mirrored tmux session", func(t *testing.T) {
		const mirrorID = "agent-mirrored"
		records[mirrorID] = &SessionRecord{
			ID:          mirrorID,
			Kind:        "agent",
			AiCli:       "claude",
			TmuxSession: mirrorID,
			Runtime: &store.SessionRuntime{
				Kind:       "tmux",
				PinnedMode: "on",
			},
		}
		require.NoError(t, m.Host().NewSession(ctx, mirrorID, t.TempDir()))
		defer func() { _ = m.Host().KillSession(ctx, mirrorID) }()

		info := m.Resolve(ctx, mirrorID)
		require.Equal(t, "tmux", info.Kind)
		require.Equal(t, "tmux", info.Presentation)
		require.Equal(t, "tmux", info.ProcessOwner)
		require.Equal(t, "live", info.Source)
		require.False(t, info.Authoritative)
		require.Contains(t, info.Capabilities, "observe")
		require.Contains(t, info.Capabilities, "tmux-attach")
	})

	// 4. Legacy record (probe): record has no runtime field, but tmux has-session succeeds
	t.Run("legacy record with live tmux session", func(t *testing.T) {
		const legacyID = "legacy-agent-live"
		records[legacyID] = &SessionRecord{
			ID:          legacyID,
			Kind:        "agent",
			AiCli:       "claude",
			TmuxSession: legacyID,
			Runtime:     nil, // legacy
		}
		m.SetSessionRuntimePin(legacyID, ModeOff)
		require.NoError(t, m.Host().NewSession(ctx, legacyID, t.TempDir()))
		defer func() { _ = m.Host().KillSession(ctx, legacyID) }()

		info := m.Resolve(ctx, legacyID)
		require.Equal(t, "tmux", info.Kind)
		require.Equal(t, "tmux", info.Presentation)
		require.Equal(t, "tmux", info.ProcessOwner)
		require.Equal(t, "legacy-probe", info.Source)
		require.False(t, info.Authoritative)
	})

	// 5. Dead / missing -> unknown with actionable reason; NEVER infers from TmuxSession != ""
	t.Run("dead legacy session never infers from TmuxSession != empty", func(t *testing.T) {
		const deadLegacyID = "legacy-agent-dead"
		records[deadLegacyID] = &SessionRecord{
			ID:          deadLegacyID,
			Kind:        "agent",
			AiCli:       "claude",
			TmuxSession: "non-existent-tmux-session-name",
			Runtime:     nil,
		}

		info := m.Resolve(ctx, deadLegacyID)
		require.Equal(t, "unknown", info.Kind)
		require.Equal(t, "none", info.ProcessOwner)
		require.NotEmpty(t, info.Reason, "must return actionable reason")
		require.Empty(t, info.Capabilities)
	})

	t.Run("dead direct session from record", func(t *testing.T) {
		const deadDirectID = "direct-dead"
		records[deadDirectID] = &SessionRecord{
			ID:          deadDirectID,
			Kind:        "terminal",
			TmuxSession: deadDirectID,
			Runtime: &store.SessionRuntime{
				Kind:       "direct",
				PinnedMode: "on",
			},
		}

		info := m.Resolve(ctx, deadDirectID)
		require.Equal(t, "direct", info.Kind)
		require.Equal(t, "record", info.Source)
		require.False(t, info.Authoritative)
		require.NotEmpty(t, info.Reason)
	})

	t.Run("unknown session id", func(t *testing.T) {
		info := m.Resolve(ctx, "completely-unknown-session-12345")
		require.Equal(t, "unknown", info.Kind)
		require.Equal(t, "none", info.ProcessOwner)
		require.NotEmpty(t, info.Reason)
	})
}

func TestValidateAdapterDirect_Matrix(t *testing.T) {
	// 8 backends from §6.5 table
	backends := []struct {
		name       string
		wantDirect bool
	}{
		{"claude", false},
		{"antigravity", false},
		{"agy", false},
		{"codex", false},
		{"cursor", false},
		{"aider", false},
		{"opencode", false},
		{"crush", false},
		{"goose", false},
		// Shell / terminal is direct-capable
		{"terminal", true},
		{"shell", true},
		{"", true},
		// Arbitrary unknown adapter
		{"unknown-ai-tool", false},
	}

	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdapterDirect(tc.name)
			if tc.wantDirect {
				require.NoError(t, err)
				require.True(t, IsAdapterDirectCapable(tc.name))
			} else {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrAdapterNotDirectCapable))
				require.False(t, IsAdapterDirectCapable(tc.name))
			}
		})
	}
}
