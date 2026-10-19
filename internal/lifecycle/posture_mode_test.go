package lifecycle

import (
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/stretchr/testify/require"
)

func TestApplyPostureBackendMode(t *testing.T) {
	cases := []struct {
		name string
		req  SpawnRequest
		want string
	}{
		{"agy bypass is native", SpawnRequest{Backend: "antigravity", PermissionPosture: ptyevent.PostureDangerouslyExecuteAll}, "dangerously-skip-permissions"},
		{"agy read-only", SpawnRequest{Backend: "antigravity", PermissionPosture: ptyevent.PostureReadOnly}, "plan"},
		{"allowlisted leaves launch untouched", SpawnRequest{Backend: "antigravity", PermissionPosture: ptyevent.PostureAllowlisted}, ""},
		{"explicit mode wins", SpawnRequest{Backend: "antigravity", PermissionMode: "accept-edits", PermissionPosture: ptyevent.PostureDangerouslyExecuteAll}, "accept-edits"},
		{"no posture no change", SpawnRequest{Backend: "antigravity"}, ""},
		{"unknown backend no change", SpawnRequest{Backend: "nope", PermissionPosture: ptyevent.PostureReadOnly}, ""},
	}
	for _, c := range cases {
		req := c.req
		applyPostureBackendMode(&req)
		require.Equal(t, c.want, req.PermissionMode, c.name)
	}
}
