package crashtest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/dbtx/workflows"
)

const journalName = "xtx.journal"

func fileSize(t testing.TB, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// committedDir returns a data dir holding the workflow's committed POST state
// (warm journal + one real commit) exactly as a crash would leave it: the
// files are copied while the DB is still open, so no clean-close checkpoint
// has folded the COMMIT record away (after a clean close the stamps are gone
// and the journal is history only). preSize/postSize bound the workflow's
// COMMIT record in xtx.journal.
func (b *base) committedDir(t testing.TB) (dir string, preSize, postSize int64) {
	t.Helper()
	live := b.fresh(t)
	preSize = fileSize(t, filepath.Join(live, journalName))
	s := mustOpen(t, live, nil)
	defer s.close()
	if err := b.w.Run(bg, s.e); err != nil {
		t.Fatalf("run: %v", err)
	}
	dir = filepath.Join(t.TempDir(), "crashed")
	copyDir(t, live, dir)
	postSize = fileSize(t, filepath.Join(dir, journalName))
	if postSize <= preSize {
		t.Fatalf("journal did not grow (%d -> %d)", preSize, postSize)
	}
	return dir, preSize, postSize
}

// TestJournalDamage (scenario 3): the journal is the single commit point, so
// damaging it must either leave an exact PRE/POST state or fail closed with a
// typed error — never a mixed state, never a panic. Observed behaviour is
// logged and recorded in the transaction-boundary spec.
func TestJournalDamage(t *testing.T) {
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepare(t, w)
			rec := b.fresh(t)
			_ = rec

			type damage struct {
				name string
				// wantPre: damage lands strictly inside the COMMIT record, so
				// the record cannot be honoured and the tx must read as PRE.
				apply   func(t *testing.T, path string, pre, post int64)
				wantPre bool
				wantErr bool // fail closed is the only acceptable outcome? (false = either)
				wantPst bool
			}
			cut := func(at func(pre, post int64) int64) func(*testing.T, string, int64, int64) {
				return func(t *testing.T, p string, pre, post int64) {
					if err := os.Truncate(p, at(pre, post)); err != nil {
						t.Fatal(err)
					}
				}
			}
			cases := []damage{
				{name: "tail-torn-1-byte", apply: cut(func(_, post int64) int64 { return post - 1 }), wantPre: true},
				{name: "tail-torn-mid-record", apply: cut(func(pre, post int64) int64 { return pre + (post-pre)/2 }), wantPre: true},
				{name: "tail-torn-header-only", apply: cut(func(pre, _ int64) int64 { return pre + 1 }), wantPre: true},
				{name: "record-removed", apply: cut(func(pre, _ int64) int64 { return pre }), wantPre: true},
				{name: "garbage-appended-after-commit", wantPst: true, apply: func(t *testing.T, p string, _, _ int64) {
					f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					if _, err := f.Write([]byte{0xde, 0xad, 0xbe}); err != nil {
						t.Fatal(err)
					}
				}},
				{name: "byte-flip-in-commit-record", apply: func(t *testing.T, p string, pre, post int64) {
					data, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					data[pre+(post-pre)/2] ^= 0xff
					if err := os.WriteFile(p, data, 0o644); err != nil {
						t.Fatal(err)
					}
				}},
				{name: "journal-deleted", apply: func(t *testing.T, p string, _, _ int64) {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				}},
				{name: "journal-zeroed", apply: func(t *testing.T, p string, _, _ int64) {
					if err := os.Truncate(p, 0); err != nil {
						t.Fatal(err)
					}
				}},
			}
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					dir, pre, post := b.committedDir(t)
					c.apply(t, filepath.Join(dir, journalName), pre, post)
					s, err := openStack(dir, nil)
					if err != nil {
						if c.wantPre || c.wantPst {
							t.Fatalf("expected a clean reopen, got: %v", err)
						}
						// Fail closed: must be a typed engine error and the
						// directory must not have been modified into garbage.
						typed := errors.Is(err, engine.ErrXTxJournalCorrupt) ||
							errors.Is(err, engine.ErrXTxJournalMissing) ||
							errors.Is(err, engine.ErrXTxRecoveryRequired) ||
							errors.Is(err, engine.ErrXTxUnsupported)
						t.Logf("OBSERVED %s/%s: fail-closed open error (typed=%v): %v", w.Name, c.name, typed, err)
						if !typed {
							t.Fatalf("untyped open error on damaged journal: %v", err)
						}
						return
					}
					snap := snapshot(t, s.db)
					st := b.classify(snap)
					s.close()
					t.Logf("OBSERVED %s/%s: reopened, state=%s", w.Name, c.name, st)
					switch {
					case st == stateMixed:
						t.Fatalf("MIXED state after journal damage %s", c.name)
					case c.wantPre && st != statePre:
						t.Fatalf("torn commit record must read as PRE, got %s", st)
					case c.wantPst && st != statePost:
						t.Fatalf("garbage after a complete commit record must keep POST, got %s", st)
					}
					// Full verification (checkers, Outcome agreement, idempotent
					// reopen, converging retry) on a state that opened cleanly.
					b.verifyReopened(t, dir, w.Name+"/"+c.name)
				})
			}
		})
	}
}

// TestFacadeFormatWithoutJournal pins finding F1: a root holding xtx.format
// but no xtx.journal (what a SIGKILL during a database's FIRST XTx commit
// leaves) opens, yet the façade refuses every Collection() call with
// ErrXTxRecoveryRequired. The test skips with the finding while the upstream
// defect is present and becomes a real assertion once it is fixed.
func TestFacadeFormatWithoutJournal(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	s := mustOpen(t, src, nil)
	warmJournal(t, s)
	s.close()

	dir := filepath.Join(t.TempDir(), "dst")
	db, err := scriva.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	data, err := os.ReadFile(filepath.Join(src, "xtx.format"))
	if err != nil {
		t.Fatalf("source has no xtx.format: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xtx.format"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err = scriva.Open(dir)
	if err != nil {
		t.Fatalf("scriva.Open must succeed on format-without-journal: %v", err)
	}
	defer db.Close()
	_, err = db.Collection("anything")
	if errors.Is(err, engine.ErrXTxRecoveryRequired) {
		t.Skipf("FINDING F1 (upstream scriva): xtx.format without xtx.journal opens but db.Collection fails permanently: %v", err)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Defect fixed upstream: the store must be fully usable and atomic.
	if err := workflows.Setup(db); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	fmt.Fprintln(os.Stderr, "F1 appears fixed upstream")
}
