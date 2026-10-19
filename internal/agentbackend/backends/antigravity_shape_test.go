package backends

import (
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/stretchr/testify/require"
)

func TestAntigravityShapePermission(t *testing.T) {
	app := &agentbackend.Approval{
		Action:  "Read: /etc/hosts",
		Options: []string{"Yes", "Yes, always allow in this conversation", "Yes (Persist to settings.json)", "No"},
	}
	sh, ok := Antigravity{}.ShapePrompt(app)
	require.True(t, ok)
	require.NoError(t, agentbackend.ValidateShape(sh, app))
	require.Equal(t, []ptyevent.Capability{ptyevent.CapRead}, sh.Caps)
	require.Equal(t, "/etc/hosts", sh.Target)
	require.Equal(t, ptyevent.SemAllowOnce, sh.Options[0].Semantic)
	require.Equal(t, ptyevent.SemAllowSession, sh.Options[1].Semantic)
	require.Equal(t, ptyevent.ScopeGlobal, sh.Options[2].Scope)
	require.Equal(t, ptyevent.SemDeny, sh.Options[3].Semantic)
}

func TestAntigravityShapeTrust(t *testing.T) {
	app := &agentbackend.Approval{
		Kind: agentbackend.ApprovalKindTrust, Action: "/work/proj",
		Options: []string{"Yes, I trust this folder", "No, exit"},
	}
	sh, ok := Antigravity{}.ShapePrompt(app)
	require.True(t, ok)
	require.Equal(t, []ptyevent.Capability{ptyevent.CapTrust}, sh.Caps)
	require.Equal(t, ptyevent.SemCancel, sh.Options[1].Semantic)
	require.Equal(t, 2, sh.Cancel.OptionIndex)
}

func TestAntigravityShapeUnknownWording(t *testing.T) {
	_, ok := Antigravity{}.ShapePrompt(&agentbackend.Approval{
		Action: "ls", Options: []string{"Yes", "Maybe later"},
	})
	require.False(t, ok, "unknown wording must not be shaped")
	_, ok = Antigravity{}.ShapePrompt(&agentbackend.Approval{Options: []string{"Yes", "No"}})
	require.False(t, ok, "no action must not be shaped")
}
