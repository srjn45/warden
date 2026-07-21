package poller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetContextGuardSwapsSnapshot proves the hot-reload setter atomically swaps
// every context/token guard knob and that ctxGuard reads them back coherently —
// the seam Server.ApplyConfig uses to apply tokens.* live without a restart.
func TestSetContextGuardSwapsSnapshot(t *testing.T) {
	p := &Poller{}

	p.SetContextGuard(true, 111, 222, true, true, false, "resume-now")

	g := p.ctxGuard()
	require.True(t, g.Guard)
	require.Equal(t, 111, g.Warn)
	require.Equal(t, 222, g.Crit)
	require.True(t, g.WarnAlert)
	require.True(t, g.AutoCompact)
	require.False(t, g.ForceCompact)
	require.Equal(t, "resume-now", g.CompactResume)

	// A second reload replaces the whole snapshot.
	p.SetContextGuard(false, 5, 9, false, false, true, "")
	g = p.ctxGuard()
	require.False(t, g.Guard)
	require.Equal(t, 5, g.Warn)
	require.True(t, g.ForceCompact)
	require.Empty(t, g.CompactResume)
}
