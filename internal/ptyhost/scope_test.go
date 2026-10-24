package ptyhost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFixtureCleanupPreservesOtherOwnershipTokens(t *testing.T) {
	r := newTmux(t)
	ctx := context.Background()
	_, err := r.Run(ctx, "", "tmux", "new-session", "-d", "-s", "ours")
	require.NoError(t, err)
	_, err = r.Run(ctx, "", "tmux", "new-session", "-d", "-s", "foreign")
	require.NoError(t, err)
	other := r
	other.token = "another-fixture"
	_, err = r.Run(ctx, "", "tmux", "set-option", "-t", "foreign", "@warden_test_owner", other.token)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.cleanup()) })
	require.NoError(t, r.cleanup())
	_, err = r.Run(ctx, "", "tmux", "has-session", "-t", "foreign")
	require.NoError(t, err, "cleanup killed a session it did not own")
	_, err = r.Run(ctx, "", "tmux", "has-session", "-t", "ours")
	require.Error(t, err)
}
