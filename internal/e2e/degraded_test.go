package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Schema 2 (warden-db) heals LYING DERIVED STATE and refuses LYING DATA. An index
// that points at the wrong record is rebuilt from the append-only segments on
// open, so the daemon serves the full, correct fleet (never a wrong or short
// one) and the segments — the truth — stay byte-identical. The schema-1 tools
// refuse on a schema-2 dir instead of touching the inert legacy copies, and
// `repair warden-db` is the offline verifier.
func TestIndexFaultsNeverServeWrongData(t *testing.T) {
	for name, inject := range faults {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.seed("a-1", "a-2", "a-3")
			inject(e)
			before := segments(t, e)

			d := e.startDaemon(freeAddr(t), e.data, e.root)
			d.waitUp()

			code, got := fleet(t, d)
			require.Equal(t, 200, code)
			require.Equal(t, []string{"tm-a-1", "tm-a-2", "tm-a-3"}, got, "daemon must serve the full, correct fleet")
			h := d.health()
			require.True(t, h.Healthy, "a healed derived index is not degradation: %+v", h)
			// single-agent lookup returns that agent's own record.
			code, body := get(t, d.base()+"/api/v1/sessions/a-1")
			require.Equal(t, 200, code, string(body))
			var s struct{ ID string }
			require.NoError(t, json.Unmarshal(body, &s))
			require.Equal(t, "a-1", s.ID, "lookup returned another agent's record")

			d.stop()
			requireSame(t, before, segments(t, e), "an index fault must never change record segments")

			cfg := e.config(freeAddr(t), e.data)
			out, code := e.run(e.root, "--config", cfg, "repair", "warden-db", "--dry-run")
			require.Zero(t, code, out)
			out, code = e.run(e.root, "--config", cfg, "repair", "agents", "--dry-run")
			require.NotZero(t, code, out)
			require.Contains(t, out, "schema 2", out)
			requireSame(t, before, segments(t, e), "repair --dry-run / refused schema-1 tools must not mutate")
		})
	}
}

// Corrupt record DATA is never healed or guessed: the daemon refuses to start
// naming the segment region, changes nothing, and the offline verifier reports
// the same damage.
func TestCorruptSegmentRefusesToStartAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2", "a-3")
	seg := filepath.Join(e.data, "warden-db", "agents", "seg_000001.ndjson")
	b, err := os.ReadFile(seg)
	require.NoError(t, err)
	at := bytes.Index(b[600:], []byte(`"name"`)) + 600
	copy(b[at:], []byte(`"na!!`))
	require.NoError(t, os.WriteFile(seg, b, 0o600))
	before := agentsDB(t, e)

	d := e.startDaemon(freeAddr(t), e.data, e.root)
	log := d.waitExit()
	require.Contains(t, log, "integrity violation", log)
	require.Contains(t, log, "nothing was modified or salvaged", log)
	requireSame(t, before, agentsDB(t, e), "a refused open must not mutate the store")

	out, _ := e.run(e.root, "--config", e.config(freeAddr(t), e.data), "repair", "warden-db", "--dry-run")
	require.Contains(t, out, "health=corrupt", "the verifier must report the injected damage")
	requireSame(t, before, agentsDB(t, e), "verification is read-only")
}

// Repair/doctor refuse while a daemon owns the store, from any alias, and the
// owning daemon keeps serving untouched.
func TestOwnedStoreRefusedAcrossAliases(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2")
	link := filepath.Join(e.root, "alias")
	require.NoError(t, exec.Command("ln", "-s", e.data, link).Run())

	owner := e.startDaemon(freeAddr(t), e.data, e.root)
	owner.waitUp()
	before := agentsDB(t, e)

	aliases := map[string]string{
		"absolute": e.data,
		"symlink":  link,
		"relative": "data",
		"dotted":   e.data + "/./",
		"parent":   e.root + "/home/../data",
	}
	for name, alias := range aliases {
		t.Run(name, func(t *testing.T) {
			// second daemon on a DIFFERENT port refuses before binding anything.
			second := e.startDaemon(freeAddr(t), alias, e.root)
			log := second.waitExit()
			require.Contains(t, log, "owned", log)
			require.Contains(t, log, "next step", log)

			cfg := e.config(freeAddr(t), alias)
			out, code := e.run(e.root, "--config", cfg, "repair", "warden-db", "--backup-dir", filepath.Join(e.root, "bk-"+name))
			require.NotZero(t, code)
			require.Contains(t, out, "owned", out)
			out, code = e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
			require.NotZero(t, code)
			require.Contains(t, out, "owned", out)

			// doctor pointed at a dead port still detects the owner by lock.
			out, _ = e.run(e.root, "--config", cfg, "doctor")
			require.Contains(t, out, "owned by another warden process", out)
		})
	}
	// the owner still serves the full fleet and nothing was mutated.
	code, body := get(t, owner.base()+"/api/v1/sessions")
	require.Equal(t, 200, code)
	require.Equal(t, 2, strings.Count(string(body), `"tmux_session"`))
	require.True(t, owner.health().Healthy)
	requireSame(t, before, agentsDB(t, e), "refused openers mutated the store")
}

// SIGKILL leaves no stale ownership: the kernel drops the flock, the next
// daemon (different port) starts, and data written before the kill is intact.
func TestLockReleasedAfterSIGKILL(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2")
	first := e.startDaemon(freeAddr(t), e.data, e.root)
	first.waitUp()
	cfg := e.config(first.addr, e.data)
	out, _ := e.run(e.root, "--config", e.config(freeAddr(t), e.data), "doctor")
	require.Contains(t, out, "owned by another warden process", out)

	first.kill()
	out, _ = e.run(e.root, "--config", cfg, "doctor")
	require.Contains(t, out, "store not owned", out)

	second := e.startDaemon(freeAddr(t), e.data, e.root)
	second.waitUp()
	require.True(t, second.health().Healthy)
	code, body := get(t, second.base()+"/api/v1/sessions")
	require.Equal(t, 200, code)
	require.Equal(t, 2, strings.Count(string(body), `"tmux_session"`))
}

// A daemon killed at arbitrary points of startup never leaves the store owned
// or damaged: record segments stay byte-identical (a lying index is derived
// state the next open simply rebuilds) and the final daemon serves the full,
// correct fleet.
func TestStartupInterruption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*env)
	}{
		{"healthy", func(*env) {}},
		{"stale-index", faults["in-range-wrong-record"]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seed("a-1", "a-2", "a-3")
			tc.inject(e)
			before := segments(t, e)
			for i := 0; i < 8; i++ {
				d := e.startDaemon(freeAddr(t), e.data, e.root)
				// kill at staggered points from "just forked" to "serving".
				for j := 0; j < i*3 && !d.exited(); j++ {
					get1(d)
				}
				d.kill()
			}
			requireSame(t, before, segments(t, e), "interrupted startups mutated record segments")
			final := e.startDaemon(freeAddr(t), e.data, e.root)
			final.waitUp()
			require.True(t, final.health().Healthy)
			code, got := fleet(t, final)
			require.Equal(t, 200, code)
			require.Equal(t, []string{"tm-a-1", "tm-a-2", "tm-a-3"}, got)
			final.stop()
			requireSame(t, before, segments(t, e), "final run mutated record segments")
		})
	}
}

func get1(d *daemon) { // one cheap, time-consuming probe to stagger the kill point
	d.e.t.Helper()
	_, _ = httpGetQuiet(d.base() + "/healthz")
}

// A primary-index entry that is simply absent is derived-state damage: the
// engine rebuilds it from the segments on open, so the daemon serves the FULL
// fleet — never a short one presented as complete.
func TestMissingIndexEntryIsHealedNotServedShort(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2", "a-3")
	e.rewriteIndex("agents", func(m map[string]idxEntry) { delete(m, byOffset(m)[2]) })
	before := segments(t, e)
	d := e.startDaemon(freeAddr(t), e.data, e.root)
	d.waitUp()
	code, got := fleet(t, d)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2", "tm-a-3"}, got, "must not present a short fleet")
	require.True(t, d.health().Healthy)
	d.stop()
	requireSame(t, before, segments(t, e), "record segments must not mutate")
}
