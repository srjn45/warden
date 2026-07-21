package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetConfigSwapsLive proves the config hot-reload seam: SetConfig atomically
// replaces the provider so subsequent reads (model_default, permission mode, the
// hint/rails gates) see the new values without a restart.
func TestSetConfigSwapsLive(t *testing.T) {
	l := New(&FakeRunner{}, &FakeConfig{ModelDefault: "opus", PermissionMode: "auto"})
	require.Equal(t, "opus", l.config().GetModelDefault())
	require.True(t, l.config().GetGitConventions(), "rails default on")

	l.SetConfig(&FakeConfig{ModelDefault: "sonnet", PermissionMode: "plan", GitConventionsOff: true})
	require.Equal(t, "sonnet", l.config().GetModelDefault())
	require.Equal(t, "plan", l.config().GetDefaultPermissionMode())
	require.False(t, l.config().GetGitConventions(), "rails toggle applied live")
}
