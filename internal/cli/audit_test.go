package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/audit"
)

// runAuditLog executes `audit log` against a temp data_dir holding the given
// pre-seeded events, returning combined stdout/stderr.
func runAuditLog(t *testing.T, events []audit.Event, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	w := audit.NewWriter(filepath.Join(dir, "audit.jsonl"))
	for _, ev := range events {
		w.Log(ev)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\n"), 0o600))

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"audit", "log", "--config", cfgPath}, args...))
	require.NoError(t, root.Execute())
	return buf.String()
}

func TestAuditLogRendersRecords(t *testing.T) {
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	out := runAuditLog(t, []audit.Event{
		{Time: base, Action: audit.ActionSpawn, Actor: "127.0.0.1", Target: "abc123", Detail: map[string]string{"name": "fixer", "repo": "/r"}},
		{Time: base.Add(time.Hour), Action: audit.ActionTerminate, Actor: "127.0.0.1", Target: "abc123"},
	})
	require.Contains(t, out, "ACTION")
	require.Contains(t, out, audit.ActionSpawn)
	require.Contains(t, out, "abc123")
	require.Contains(t, out, "name=fixer repo=/r") // detail rendered as sorted k=v
}

func TestAuditLogEmpty(t *testing.T) {
	out := runAuditLog(t, nil)
	require.Contains(t, out, "no audit records match")
}

func TestAuditLogFilterByAction(t *testing.T) {
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	out := runAuditLog(t, []audit.Event{
		{Time: base, Action: audit.ActionSpawn, Target: "a1"},
		{Time: base.Add(time.Minute), Action: audit.ActionTerminate, Target: "a1"},
	}, "--action", audit.ActionTerminate)
	require.Contains(t, out, audit.ActionTerminate)
	require.NotContains(t, out, audit.ActionSpawn)
}

func TestAuditLogTailKeepsMostRecent(t *testing.T) {
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	out := runAuditLog(t, []audit.Event{
		{Time: base, Action: audit.ActionSpawn, Target: "old-agent"},
		{Time: base.Add(time.Minute), Action: audit.ActionSpawn, Target: "new-agent"},
	}, "--tail", "1")
	require.Contains(t, out, "new-agent")
	require.NotContains(t, out, "old-agent")
}

func TestAuditLogJSON(t *testing.T) {
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	out := runAuditLog(t, []audit.Event{
		{Time: base, Action: audit.ActionApprove, Target: "a1", Detail: map[string]string{"option": "2"}},
	}, "--json")
	require.Contains(t, out, `"action": "approve"`)
	require.Contains(t, out, `"option": "2"`)
}

func TestAuditLogBadSince(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"audit", "log", "--since", "garbage"})
	require.Error(t, root.Execute())
}
