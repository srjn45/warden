package backends

import (
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/stretchr/testify/require"
)

var allPostures = []ptyevent.Posture{
	ptyevent.PostureReadOnly,
	ptyevent.PostureReadWrite,
	ptyevent.PostureAllowlisted,
	ptyevent.PostureDangerouslyExecuteAll,
}

// TestPostureAudit pins the (backend × posture) mapping: every pair is either a
// native mode or an explicitly degraded one with a reason. Nothing is silently
// approximated, and allowlisted is never "native" (it is warden's rule engine).
func TestPostureAudit(t *testing.T) {
	for _, id := range []string{"claude", "codex", "antigravity", "cursor", "aider", "opencode", "crush", "goose"} {
		b := mustGet(t, id)
		for _, p := range allPostures {
			s := agentbackend.SupportForPosture(b, p)
			require.Equal(t, p, s.Posture, "%s/%s", id, p)
			if s.Degraded {
				require.False(t, s.Native, "%s/%s degraded must not be native", id, p)
				require.NotEmpty(t, s.Note, "%s/%s degraded needs a note", id, p)
			}
			if p == ptyevent.PostureAllowlisted {
				require.False(t, s.Native, "%s allowlisted is never native", id)
			}
		}
	}
}

func TestPostureAuditExactModes(t *testing.T) {
	cases := []struct {
		id      string
		p       ptyevent.Posture
		mode    string
		native  bool
		degrade bool
	}{
		{"claude", ptyevent.PostureReadOnly, "plan", true, false},
		{"claude", ptyevent.PostureReadWrite, "acceptEdits", true, false},
		{"claude", ptyevent.PostureDangerouslyExecuteAll, "bypassPermissions", true, false},
		{"codex", ptyevent.PostureReadOnly, "read-only", true, false},
		{"codex", ptyevent.PostureDangerouslyExecuteAll, "danger-full-access", true, false},
		{"antigravity", ptyevent.PostureReadOnly, "plan", true, false},
		{"antigravity", ptyevent.PostureReadWrite, "accept-edits", true, false},
		{"antigravity", ptyevent.PostureDangerouslyExecuteAll, "dangerously-skip-permissions", true, false},
		{"antigravity", ptyevent.PostureAllowlisted, "default", false, true},
	}
	for _, c := range cases {
		s := agentbackend.SupportForPosture(mustGet(t, c.id), c.p)
		require.Equal(t, c.mode, s.Mode, "%s/%s", c.id, c.p)
		require.Equal(t, c.native, s.Native, "%s/%s", c.id, c.p)
		require.Equal(t, c.degrade, s.Degraded, "%s/%s", c.id, c.p)
	}
}
