package e2e

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
)

// Every degraded surface must agree: lookup, list, tree, SSE, health, doctor
// all say "degraded, fail closed" — none serves a partial fleet as complete —
// and no process mutates the store while reporting it.
func TestDegradedStoreSurfacesAgree(t *testing.T) {
	for name, inject := range faults {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.seed("a-1", "a-2", "a-3")
			inject(e)
			before := agentsDB(t, e)

			d := e.startDaemon(freeAddr(t), e.data, e.root)
			d.waitUp()

			h := d.health()
			require.False(t, h.Healthy, "health must be unhealthy")
			require.True(t, h.Degraded)
			require.Positive(t, h.FailureCount)
			require.Equal(t, agentstore.RepairAvailable, h.RepairAvailable)

			// list, lookup and tree fail closed with 503 — never a partial 200.
			for _, p := range []string{"/api/v1/sessions", "/api/v1/tree"} {
				code, body := get(t, d.base()+p)
				require.Equal(t, 503, code, "%s: %s", p, body)
			}
			// SSE must not stream a (partial) fleet as if complete: it emits no
			// sessions frame (silence or an error), never a subset of agents.
			_, frame := firstSSE(t, d.base()+"/api/v1/events/stream")
			require.NotContains(t, frame, `"id":"a-`, "SSE streamed agents from a degraded store")
			// single-agent lookup of the corrupted key must not return a wrong record.
			if code, body := get(t, d.base()+"/api/v1/sessions/a-1"); code == 200 {
				var s struct{ ID string }
				require.NoError(t, json.Unmarshal(body, &s))
				require.Equal(t, "a-1", s.ID, "lookup returned another agent's record")
			}

			// doctor (daemon up) reports DEGRADED but is not a hard failure.
			out, _ := e.run(e.root, "--config", e.config(d.addr, e.data), "doctor")
			require.Contains(t, out, "DEGRADED", out)

			d.stop()
			requireSame(t, before, agentsDB(t, e), "degraded daemon run must not mutate agents-db")

			// offline: doctor probes ownership only; repair reports unavailable.
			cfg := e.config(freeAddr(t), e.data)
			out, _ = e.run(e.root, "--config", cfg, "doctor")
			require.Contains(t, out, "store not owned", out)
			out, code := e.run(e.root, "--config", cfg, "repair", "agents", "--dry-run")
			require.Zero(t, code, out)
			require.Contains(t, out, "no files changed", out)
			require.NotContains(t, out, "0 finding(s)", "dry-run must report the injected damage")
			requireSame(t, before, agentsDB(t, e), "doctor/repair must not mutate agents-db")
		})
	}
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
			out, code := e.run(e.root, "--config", cfg, "repair", "agents")
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
// or damaged: a healthy store stays healthy and a degraded one stays
// byte-identical (no half-applied "repair").
func TestStartupInterruption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*env)
	}{
		{"healthy", func(*env) {}},
		{"degraded", faults["in-range-wrong-record"]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seed("a-1", "a-2", "a-3")
			tc.inject(e)
			before := agentsDB(t, e)
			for i := 0; i < 8; i++ {
				d := e.startDaemon(freeAddr(t), e.data, e.root)
				// kill at staggered points from "just forked" to "serving".
				for j := 0; j < i*3 && !d.exited(); j++ {
					get1(d)
				}
				d.kill()
			}
			if tc.name == "degraded" {
				requireSame(t, before, agentsDB(t, e), "interrupted startups mutated agents-db")
			}
			final := e.startDaemon(freeAddr(t), e.data, e.root)
			final.waitUp()
			require.Equal(t, tc.name == "healthy", final.health().Healthy)
			final.stop()
			if tc.name == "degraded" {
				requireSame(t, before, agentsDB(t, e), "final run mutated agents-db")
			}
		})
	}
}

func get1(d *daemon) { // one cheap, time-consuming probe to stagger the kill point
	d.e.t.Helper()
	_, _ = httpGetQuiet(d.base() + "/healthz")
}

// A primary-index entry that is simply absent is now caught by full ScrivaDB
// Verify (srjn45/scriva#107), so the degraded store fails closed instead of
// reading as a complete, healthy fleet of the remaining agents.
func TestMissingIndexEntryIsDetected(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2", "a-3")
	e.rewriteIndex("agents", func(m map[string]idxEntry) { delete(m, byOffset(m)[2]) })
	before := agentsDB(t, e)
	d := e.startDaemon(freeAddr(t), e.data, e.root)
	d.waitUp()
	code, body := get(t, d.base()+"/api/v1/sessions")
	require.Equal(t, 503, code, string(body))
	require.Zero(t, strings.Count(string(body), `"tmux_session"`), "degraded store must not present a short fleet")
	require.False(t, d.health().Healthy)
	d.stop()
	requireSame(t, before, agentsDB(t, e), "read-only run must not mutate")
}
