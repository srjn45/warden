package crashtest

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/dbtx/workflows"
)

// killPlan says when the parent SIGKILLs the helper.
type killPlan struct {
	name   string
	mode   string        // helper mode
	marker string        // wait for this stdout line first ("" = none)
	delay  time.Duration // then sleep this long before the kill
}

func plans() []killPlan {
	ps := []killPlan{
		{"before-commit", modePreCommit, "STAGED", 0},
		{"after-commit", modePostCommit, "ACK", 0},
		{"segment-growth", modeFree, "SEGGROWN", 0},
		{"journal-growth", modeFree, "JGROWN", 0},
	}
	for _, d := range []time.Duration{0, 500 * time.Microsecond} {
		ps = append(ps, killPlan{fmt.Sprintf("seg+%s", d), modeFree, "SEGGROWN", d})
		ps = append(ps, killPlan{fmt.Sprintf("journal+%s", d), modeFree, "JGROWN", d})
	}
	rng := rand.New(rand.NewSource(20261010))
	for i := 0; i < scale(4, 60); i++ {
		d := time.Duration(rng.Intn(6000)) * time.Microsecond
		ps = append(ps, killPlan{fmt.Sprintf("rand-%02d+%s", i, d), modeFree, "RUN", d})
	}
	return ps
}

func TestRealProcessCrash(t *testing.T) {
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepare(t, w)
			var mu sync.Mutex
			tally := map[string]int{}
			for _, p := range plans() {
				dir := b.fresh(t)
				c := startChild(t, dir, w.Name, p.mode)
				if p.marker != "" && !c.waitFor(p.marker, 30*time.Second) {
					// Marker never came: the commit finished (or died) before the
					// watcher fired. Still a valid crash point (after commit).
					if _, ok := has(c.seen, "DONE"); !ok {
						t.Fatalf("%s: helper neither hit %q nor finished; stderr: %s; saw %v", p.name, p.marker, c.errb.String(), c.seen)
					}
				}
				if p.delay > 0 {
					time.Sleep(p.delay)
				}
				lines := c.kill()
				if l, bad := has(lines, "ERR"); bad {
					t.Fatalf("%s: helper error: %s", p.name, l)
				}
				label := w.Name + "/" + p.name
				st := b.verifyReopened(t, dir, label)
				_, acked := has(lines, "ACK")
				if acked {
					if st != statePost {
						t.Fatalf("%s: dbtx reported success but state is %s after reopen", label, st)
					}
				}
				if f, failed := has(lines, "FAIL"); failed {
					if st == statePost && !strings.Contains(f, "class=outcome_unknown") {
						t.Fatalf("%s: dbtx reported final failure (%s) but the tx is present after reopen without outcome_unknown", label, f)
					}
				}
				if p.name == "before-commit" && st != statePre {
					t.Fatalf("%s: kill before CommitXTx left %s", label, st)
				}
				if p.name == "after-commit" && st != statePost {
					t.Fatalf("%s: kill after ack left %s", label, st)
				}
				mu.Lock()
				tally[st.String()]++
				mu.Unlock()
			}
			t.Logf("%s: %d kill points -> %v", w.Name, len(plans()), tally)
		})
	}
}

// TestFirstCommitCrashWindow kills the helper around the database's FIRST
// cross-collection commit, where xtx.format and xtx.journal are created. A
// crash between the two leaves a dir that scriva.Open accepts but whose façade
// Collection() refuses (finding F1). All-or-nothing must still hold.
func TestFirstCommitCrashWindow(t *testing.T) {
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepareOpt(t, w, false)
			n := scale(12, 80)
			for i := 0; i < n; i++ {
				dir := b.fresh(t)
				c := startChild(t, dir, w.Name, modeFree)
				if !c.waitFor("RUN", 30*time.Second) {
					t.Fatalf("helper never started: %s", c.errb.String())
				}
				time.Sleep(time.Duration(i*150) * time.Microsecond)
				lines := c.kill()
				st := b.verifyReopened(t, dir, fmt.Sprintf("%s/first-%02d", w.Name, i))
				if _, acked := has(lines, "ACK"); acked && st != statePost {
					t.Fatalf("%s/first-%02d: acked but %s", w.Name, i, st)
				}
			}
		})
	}
}
