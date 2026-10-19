package ptyhost

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
)

func TestDurablePin_GlobalModeFlipRestartAndSameNameReuse(t *testing.T) {
	noLeaks(t)
	run := newTmux(t)
	dir := filepath.Join(t.TempDir(), "pty")
	m := newManager(t, run, dir, ModeOn)
	defer m.Close()

	ctx := context.Background()
	name := "term-durable-pin"
	th := m.TerminalHost()

	// 1. Launch session with global ModeOn: pinned to ModeOn
	require.NoError(t, th.NewSession(ctx, name, t.TempDir()))
	pin, ok := m.PinnedRuntime(name)
	require.True(t, ok)
	require.Equal(t, ModeOn, pin)
	require.True(t, m.IsAuthoritative(name))

	// 2. Flipping global mode off -> shadow -> on does not change pin or authority
	for _, targetMode := range []Mode{ModeOff, ModeShadow, ModeOn} {
		m.SetMode(targetMode)
		require.Equal(t, targetMode, m.Mode())

		// Live session pin & authority stay unchanged
		p, ok := m.PinnedRuntime(name)
		require.True(t, ok)
		require.Equal(t, ModeOn, p)
		require.True(t, m.IsAuthoritative(name))
		require.NotNil(t, m.Direct(name))
	}

	// 3. Restart simulation:
	// Simulate durable record that stored pinned_mode: "on"
	record := &SessionRecord{
		ID:          name,
		Kind:        "terminal",
		TmuxSession: name,
		Runtime: &store.SessionRuntime{
			Kind:       "direct",
			PinnedMode: "on",
		},
	}

	// Create new manager instance m2 with global mode ModeOff
	m2 := New(Config{
		Mode:  ModeOff,
		Dir:   dir,
		Run:   run,
		Shell: []string{"/bin/sh"},
		Options: ptyruntime.Options{
			IdleFlush:  20 * time.Millisecond,
			KillGrace:  300 * time.Millisecond,
			DrainGrace: 200 * time.Millisecond,
		},
	})
	defer m2.Close()
	m2.SetTerminalResolver(func(n string) bool { return n == name })
	m2.SetRecordResolver(func(ctx context.Context, id string) (*SessionRecord, bool) {
		if id == name {
			return record, true
		}
		return nil, false
	})

	// Boot reload: reload pin from record into m2
	m2.SetSessionRuntimePin(record.RuntimeKey(), ParseMode(record.Runtime.PinnedMode))

	// Verify pin and effective mode in m2 even though global mode is ModeOff
	p2, ok2 := m2.PinnedRuntime(name)
	require.True(t, ok2)
	require.Equal(t, ModeOn, p2)
	require.Equal(t, ModeOn, m2.EffectiveMode(name))
	info2 := m2.Resolve(ctx, name)
	require.Equal(t, "on", info2.PinnedMode)
	require.Equal(t, "off", info2.RequestedMode)

	// 4. Same-name reuse test:
	// Terminate the old session
	require.NoError(t, th.KillSession(ctx, name))

	// Now in m (with global ModeOff): launch a new session with the same name
	m.SetMode(ModeOff)
	thOff := m.TerminalHost()
	require.NoError(t, thOff.NewSession(ctx, name, t.TempDir()))

	// Stale pin was cleared; new session under ModeOff runs in tmux and is not direct or authoritative
	pinReuse, okReuse := m.PinnedRuntime(name)
	require.True(t, okReuse)
	require.Equal(t, ModeOff, pinReuse, "new session with same name gets fresh pin reflecting launch conditions")
	require.False(t, m.IsAuthoritative(name))
	require.Nil(t, m.Direct(name))
}
