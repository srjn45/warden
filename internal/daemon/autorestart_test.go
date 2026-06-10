package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecideRestart(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const max = 3
	const reset = 5 * time.Minute

	// First-ever crash: no prior restart -> restart, count 1.
	act, next := decideRestart(0, time.Time{}, now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Recent restart, below cap -> restart, count+1.
	act, next = decideRestart(1, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 2, next)

	// At cap, recent -> give up.
	act, _ = decideRestart(3, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionGiveUp, act)

	// At cap but sustained-healthy (>= reset since last) -> reset -> restart, count 1.
	act, next = decideRestart(3, now.Add(-6*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: exactly reset elapsed -> resets.
	act, next = decideRestart(3, now.Add(-5*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: just under reset, at cap -> give up.
	act, _ = decideRestart(3, now.Add(-(5*time.Minute - time.Second)), now, max, reset)
	require.Equal(t, actionGiveUp, act)
}
