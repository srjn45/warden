package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
)

// seedLegacy writes a schema-1 data dir through the real compat stores: two LIVE
// agents (with live tmux sessions) and one archived agent. Temp env only.
func (e *env) seedLegacy() {
	e.t.Helper()
	require.NoError(e.t, schema.Save(e.data, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1", History: []schema.HistoryEntry{}}))
	s, err := storage.OpenLegacy(e.data)
	require.NoError(e.t, err)
	ctx := context.Background()
	for _, id := range []string{"a-1", "a-2"} {
		require.NoError(e.t, s.Agents.Insert(ctx, &agentstore.Agent{ID: id, Name: "n-" + id, Status: store.StatusWorking, PID: 4242,
			TmuxSession: "tm-" + id, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	}
	require.NoError(e.t, s.Agents.Insert(ctx, &agentstore.Agent{ID: "a-old", Name: "old", Status: store.StatusDone, TmuxSession: "tm-a-old",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(e.t, s.Agents.Archive(ctx, "a-old"))
	require.NoError(e.t, s.Close())
}

// The whole upgrade as the operator sees it, with real binaries: a schema-1
// daemon-less data dir is refused by the new daemon, planned read-only, cut over
// once, then served on schema 2 with the live agents' tmux sessions untouched;
// an apply beside the running daemon is refused and nothing in the legacy
// stores changed.
func TestCutoverThenSchema2DaemonServesTheSameFleet(t *testing.T) {
	e := newEnv(t)
	e.seedLegacy()
	for _, id := range []string{"a-1", "a-2"} {
		require.NoError(t, e.tmux("new-session", "-d", "-s", "tm-"+id, "sleep", "600"))
	}
	panes := e.tmuxPanePIDs()
	legacyBefore := snapshot(t, filepath.Join(e.data, "agents-db"))
	cfg := e.config(freeAddr(t), e.data)

	// 1. the schema-2 daemon refuses schema-1 data before opening any store.
	d := e.startDaemon(freeAddr(t), e.data, e.root)
	log := d.waitExit()
	require.Contains(t, log, "must be cut over to the single warden-db", log)
	require.False(t, schema.WardenDBPresent(e.data))

	// 1b. what an UPDATER that predates the cutover sees (it runs this exact
	// command on the target binary before swapping anything): non-clean, machine
	// readable, carrying the pending marker, so it blocks
	// before any change instead of swapping into a daemon that cannot boot.
	out, code := e.run(e.root, "--config", cfg, "migrate", "--check", "--target", "2", "--json", "--data-dir", e.data)
	require.NotZero(t, code, out)
	var rep struct {
		Clean    bool `json:"clean"`
		Findings []struct {
			Severity string `json:"severity"`
			Class    string `json:"class"`
		} `json:"findings"`
	}
	// combined output ends with the CLI's "error: …" line; an old updater that
	// cannot parse it falls back to blocking on the raw output, which carries the
	// same message and command.
	require.NoError(t, json.NewDecoder(strings.NewReader(out)).Decode(&rep), out)
	require.False(t, rep.Clean)
	var pending bool
	for _, f := range rep.Findings {
		pending = pending || (f.Class == "schema2_cutover_pending" && f.Severity == "repairable")
		require.NotEqual(t, "blocking", f.Severity, "a healthy schema-1 dir has no blocker: %+v", f)
	}
	require.True(t, pending, out)

	// 2. plan is read-only and ready.
	out, code = e.run(e.root, "--config", cfg, "migrate", "--plan", "--target", "2")
	require.Zero(t, code, out)
	require.Contains(t, out, "ready")
	require.False(t, schema.WardenDBPresent(e.data))

	// 3. apply (daemon stopped): the cutover runs once; a re-apply is a no-op.
	out, code = e.run(e.root, "--config", cfg, "migrate", "--apply", "--target", "2")
	require.Zero(t, code, out)
	l, err := schema.Load(e.data)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	out, code = e.run(e.root, "--config", cfg, "migrate", "--apply", "--target", "2")
	require.Zero(t, code, "re-apply on schema 2 is a no-op: %s", out)

	// 4. The released schema-2 waypoint serves the historical generation. The
	// current format-3 binary must not silently take authority over it.
	live := e.startDaemonBin(waypointBinary(t), freeAddr(t), e.data, e.root)
	live.waitUp()
	code2, got := fleet(t, live)
	require.Equal(t, 200, code2)
	require.Equal(t, []string{"tm-a-1", "tm-a-2"}, got, "active agents survive; the archived one is not in the live listing")
	var hz struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schema_version"`
	}
	_, body := get(t, live.base()+"/healthz")
	require.NoError(t, json.Unmarshal(body, &hz), string(body))
	require.Equal(t, "ok", hz.Status)
	require.Equal(t, schema.Schema2, hz.SchemaVersion)

	// 5. apply beside the running daemon is refused and changes nothing.
	out, code = e.run(e.root, "--config", cfg, "migrate", "--apply", "--target", "2")
	require.NotZero(t, code, out)
	require.Contains(t, out, "daemon is running", out)
	live.stop()

	// legacy stores byte-identical, tmux sessions untouched.
	requireSame(t, legacyBefore, snapshot(t, filepath.Join(e.data, "agents-db")), "the cutover must not change a legacy store byte")
	require.Equal(t, panes, e.tmuxPanePIDs())
	_, statErr := os.Stat(filepath.Join(e.data, "agents-db"))
	require.NoError(t, statErr, "legacy stores are kept (cleanup is post-adoption)")
}
