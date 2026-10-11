package crashtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/dbtx/workflows"
)

var bg = context.Background()

// soak enables the heavy iteration counts (WARDEN_CRASH_SOAK=1).
func soak() bool { return os.Getenv("WARDEN_CRASH_SOAK") != "" }

func scale(normal, heavy int) int {
	if soak() {
		return heavy
	}
	return normal
}

func fastBackoff(int) time.Duration { return time.Millisecond }

// recorder is a dbtx.Observer remembering every finished commit.
type recorder struct {
	mu sync.Mutex
	ev []dbtx.Event
}

func (r *recorder) Observe(e dbtx.Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }
func (r *recorder) events() []dbtx.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]dbtx.Event(nil), r.ev...)
}
func (r *recorder) last() dbtx.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ev) == 0 {
		return dbtx.Event{}
	}
	return r.ev[len(r.ev)-1]
}

// stack is one open DB plus boundary and workflow env.
type stack struct {
	db  *scriva.DB
	b   *dbtx.Boundary
	e   *workflows.Env
	rec *recorder
}

func (s *stack) close() { _ = s.db.Close() }

// openStack opens dir, runs Setup and binds a boundary over eng (nil = real
// engine; otherwise wrap receives the real Engine and returns the seam).
func openStack(dir string, wrap func(dbtx.Engine) dbtx.Engine, opts ...dbtx.Option) (*stack, error) {
	db, err := scriva.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := workflows.Setup(db); err != nil {
		db.Close()
		return nil, err
	}
	rec := &recorder{}
	opts = append([]dbtx.Option{dbtx.WithBackoff(fastBackoff), dbtx.WithObserver(rec)}, opts...)
	var b *dbtx.Boundary
	if wrap != nil {
		b, err = dbtx.NewWithEngine(wrap(dbtx.FromEngine(db.Engine())), opts...)
	} else {
		b, err = dbtx.New(db, opts...)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	e, err := workflows.NewEnv(db, b)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &stack{db: db, b: b, e: e, rec: rec}, nil
}

func mustOpen(t testing.TB, dir string, wrap func(dbtx.Engine) dbtx.Engine, opts ...dbtx.Option) *stack {
	t.Helper()
	s, err := openStack(dir, wrap, opts...)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return s
}

func snapshot(t testing.TB, db *scriva.DB) string {
	t.Helper()
	s, err := workflows.Snapshot(db)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// checkAll runs every workflow's checker (each holds on any state).
func checkAll(db *scriva.DB) error {
	for _, w := range workflows.All() {
		if err := w.Check(db); err != nil {
			return fmt.Errorf("%s: %w", w.Name, err)
		}
	}
	return nil
}

func copyDir(t testing.TB, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if filepath.Base(p) == "LOCK" {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

// base is the reference for one workflow: the seeded directory plus the exact
// pre- and post-state snapshots and the commit key a clean run produces.
type base struct {
	w    workflows.Workflow
	seed string // closed, seeded data dir (never mutated)
	pre  string
	post string
	key  string
}

// warmJournal commits one tiny cross-collection transaction over two scratch
// collections outside the workflow schema (invisible to Snapshot and every
// checker) so xtx.format and xtx.journal both exist durably. Without it the
// workflow's own commit is the database's FIRST XTx, whose crash window is
// covered separately (TestFirstCommitCrashWindow, finding F1).
func warmJournal(t testing.TB, s *stack) {
	t.Helper()
	for _, n := range []string{"warm_a", "warm_b"} {
		if _, err := s.db.Collection(n); err != nil {
			t.Fatal(err)
		}
	}
	a, err := dbtx.Bind[map[string]any](s.b, "warm_a", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := dbtx.Bind[map[string]any](s.b, "warm_b", dbtx.MapCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.b.Run(bg, "warm", func(_ context.Context, tx *dbtx.Tx) error {
		if _, err := a.Insert(tx, map[string]any{"k": "a"}); err != nil {
			return err
		}
		_, err := c.Insert(tx, map[string]any{"k": "b"})
		return err
	}); err != nil {
		t.Fatalf("warm journal: %v", err)
	}
}

func prepare(t testing.TB, w workflows.Workflow) *base { return prepareOpt(t, w, true) }

func prepareOpt(t testing.TB, w workflows.Workflow, warm bool) *base {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed")
	s := mustOpen(t, seed, nil)
	if err := w.Seed(bg, s.e); err != nil {
		t.Fatalf("%s seed: %v", w.Name, err)
	}
	if warm {
		warmJournal(t, s)
	}
	if err := w.Check(s.db); err != nil {
		t.Fatalf("%s: checker fails after seed: %v", w.Name, err)
	}
	s.close()
	s = mustOpen(t, seed, nil) // snapshot a reopened state
	pre := snapshot(t, s.db)
	s.close()

	ref := filepath.Join(t.TempDir(), "ref")
	copyDir(t, seed, ref)
	r := mustOpen(t, ref, nil)
	defer r.close()
	if err := w.Run(bg, r.e); err != nil {
		t.Fatalf("%s reference run: %v", w.Name, err)
	}
	post := snapshot(t, r.db)
	if post == pre {
		t.Fatalf("%s: reference run changed nothing", w.Name)
	}
	key := r.rec.last().Key
	if key == "" {
		t.Fatalf("%s: no commit key observed", w.Name)
	}
	return &base{w: w, seed: seed, pre: pre, post: post, key: key}
}

// fresh copies the seeded dir to a new temp dir.
func (b *base) fresh(t testing.TB) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "data")
	copyDir(t, b.seed, d)
	return d
}

type state int

const (
	statePre state = iota
	statePost
	stateMixed
)

func (s state) String() string { return [...]string{"PRE", "POST", "MIXED"}[s] }

func (b *base) classify(snap string) state {
	switch snap {
	case b.pre:
		return statePre
	case b.post:
		return statePost
	}
	return stateMixed
}

// verifyReopened is the common post-condition for every adversarial scenario:
// the dir reopens, the state is exactly PRE or POST, every checker holds,
// Outcome(key) agrees with the state, reopening is stable, and re-running the
// workflow converges to POST. It returns the state observed.
func (b *base) verifyReopened(t testing.TB, dir, label string, altKeys ...string) state {
	t.Helper()
	s, err := openStack(dir, nil)
	if errors.Is(err, engine.ErrXTxRecoveryRequired) {
		// FINDING F1 (upstream scriva): see TestFacadeFormatWithoutJournal.
		return b.verifyDegraded(t, dir, label)
	}
	if err != nil {
		t.Fatalf("%s: reopen failed (recovery must succeed or fail closed with a typed error): %v", label, err)
	}
	defer func() { s.close() }()
	snap := snapshot(t, s.db)
	st := b.classify(snap)
	if st == stateMixed {
		t.Fatalf("%s: MIXED state (neither exact pre nor exact post):\n--- pre\n%s--- post\n%s--- got\n%s", label, b.pre, b.post, snap)
	}
	if err := checkAll(s.db); err != nil {
		t.Fatalf("%s: checker failed in %s state: %v", label, st, err)
	}
	out, _ := s.b.Outcome(b.key)
	if st == statePost && out != dbtx.OutcomeCommitted {
		// A retried run re-reserves ids, so its payload (hence key) differs
		// from the reference run's; accept the key it actually committed.
		for _, k := range altKeys {
			if o, _ := s.b.Outcome(k); o == dbtx.OutcomeCommitted {
				out = o
			}
		}
	}
	switch st {
	case statePost:
		if out != dbtx.OutcomeCommitted {
			t.Fatalf("%s: state is POST but Outcome(key)=%s, want COMMITTED", label, out)
		}
	case statePre:
		if out == dbtx.OutcomeCommitted || out == dbtx.OutcomePending {
			t.Fatalf("%s: state is PRE but Outcome(key)=%s", label, out)
		}
	}
	// Reopen idempotence: byte-stable logical state across several cycles.
	for i := 0; i < scale(2, 5); i++ {
		s.close()
		if s, err = openStack(dir, nil); err != nil {
			t.Fatalf("%s: reopen #%d: %v", label, i+1, err)
		}
		if got := snapshot(t, s.db); got != snap {
			t.Fatalf("%s: state drifted on reopen #%d", label, i+1)
		}
		if err := checkAll(s.db); err != nil {
			t.Fatalf("%s: checker after reopen #%d: %v", label, i+1, err)
		}
	}
	// Retry converges (idempotent key replay or a fresh commit after abort).
	if err := b.w.Run(bg, s.e); err != nil {
		t.Fatalf("%s: re-run after recovery from %s: %v", label, st, err)
	}
	if got := snapshot(t, s.db); got != b.post {
		t.Fatalf("%s: re-run after %s did not converge to POST", label, st)
	}
	if err := checkAll(s.db); err != nil {
		t.Fatalf("%s: checker after convergence: %v", label, err)
	}
	return st
}

// f1Hits counts reopen attempts that hit finding F1 (reported in test logs).
var f1Hits atomic.Int64

// verifyDegraded handles finding F1: scriva.Open succeeded but the façade's
// Collection() refuses (ErrXTxRecoveryRequired) because xtx.format exists
// without xtx.journal. All-or-nothing must still hold at the engine level, so
// the state is verified without the façade; the retry/convergence step cannot
// run and is the defect's blast radius.
func (b *base) verifyDegraded(t testing.TB, dir, label string) state {
	t.Helper()
	f1Hits.Add(1)
	db, err := scriva.Open(dir)
	if err != nil {
		t.Fatalf("%s: F1 dir does not even open at engine level: %v", label, err)
	}
	defer db.Close()
	snap := snapshot(t, db)
	st := b.classify(snap)
	if st == stateMixed {
		t.Fatalf("%s: F1 dir holds a MIXED state", label)
	}
	if err := checkAll(db); err != nil {
		t.Fatalf("%s: F1 dir checker: %v", label, err)
	}
	if st != statePre {
		t.Fatalf("%s: F1 dir (no journal) holds state %s, want PRE", label, st)
	}
	return st
}

// ---- helper subprocess ----------------------------------------------------

const (
	envHelper   = "CRASHTEST_HELPER"
	envDir      = "CRASHTEST_DIR"
	envWorkflow = "CRASHTEST_WORKFLOW"
	envMode     = "CRASHTEST_MODE"
)

// Helper modes: what the subprocess waits for before/after the commit.
const (
	modePreCommit  = "precommit"  // block after staging, before CommitXTx
	modePostCommit = "postcommit" // block after Run returned
	modeFree       = "free"       // run straight through, then block
)

func TestMain(m *testing.M) {
	if os.Getenv(envHelper) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

func say(format string, a ...any) { fmt.Fprintf(os.Stdout, format+"\n", a...) }

// helperMain runs Seed-less: the parent already seeded the dir. It reports
// progress markers on stdout and parks forever when it has nothing left to do,
// so the parent decides precisely when the SIGKILL lands.
func helperMain() {
	dir, name, mode := os.Getenv(envDir), os.Getenv(envWorkflow), os.Getenv(envMode)
	var w *workflows.Workflow
	for _, c := range workflows.All() {
		if c.Name == name {
			c := c
			w = &c
		}
	}
	if w == nil {
		say("ERR unknown workflow %q", name)
		os.Exit(2)
	}
	s, err := openStack(dir, nil)
	if err != nil {
		say("ERR open: %v", err)
		os.Exit(2)
	}
	if mode == modePreCommit {
		s.e.BeforeCommit = func(int) error {
			say("STAGED")
			select {}
		}
	}
	// Size watcher: report the first growth of any data file so the parent can
	// kill inside the S1..S4 window of the commit.
	go watchGrowth(dir)
	say("RUN")
	err = w.Run(bg, s.e)
	ev := s.rec.last()
	if err != nil {
		oc := ""
		if be, ok := err.(*dbtx.Error); ok {
			oc = string(be.Outcome)
		}
		say("FAIL class=%s outcome=%s key=%s", dbtx.Classify(err), oc, ev.Key)
	} else {
		say("ACK key=%s", ev.Key)
	}
	say("DONE")
	select {}
}

func watchGrowth(dir string) {
	var segs []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(p) != "LOCK" && filepath.Base(p) != "xtx.journal" {
			segs = append(segs, p)
		}
		return nil
	})
	size := func(p string) int64 {
		if fi, err := os.Stat(p); err == nil {
			return fi.Size()
		}
		return -1
	}
	jp := filepath.Join(dir, "xtx.journal")
	base := make([]int64, len(segs))
	for i, p := range segs {
		base[i] = size(p)
	}
	j0 := size(jp)
	segSeen, jSeen := false, false
	for {
		if !segSeen {
			for i, p := range segs {
				if size(p) != base[i] {
					segSeen = true
					say("SEGGROWN")
					break
				}
			}
		}
		if !jSeen && size(jp) != j0 {
			jSeen = true
			say("JGROWN")
		}
		if segSeen && jSeen {
			return
		}
		time.Sleep(10 * time.Microsecond)
	}
}

// child is a running helper subprocess.
type child struct {
	cmd   *exec.Cmd
	lines chan string
	seen  []string
	errb  *strings.Builder
}

func startChild(t testing.TB, dir, workflow, mode string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envHelper+"=1", envDir+"="+dir, envWorkflow+"="+workflow, envMode+"="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	eb := &strings.Builder{}
	cmd.Stderr = eb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	c := &child{cmd: cmd, lines: make(chan string, 64), errb: eb}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return c
}

// waitFor blocks until a line with the given prefix arrives (ok=false if the
// child exited or the timeout hit first).
func (c *child) waitFor(prefix string, timeout time.Duration) bool {
	tm := time.After(timeout)
	for _, l := range c.seen {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	for {
		select {
		case l, ok := <-c.lines:
			if !ok {
				return false
			}
			c.seen = append(c.seen, l)
			if strings.HasPrefix(l, prefix) {
				return true
			}
			if l == "DONE" { // commit finished without the marker appearing
				return false
			}
		case <-tm:
			return false
		}
	}
}

// kill SIGKILLs the child and returns every line it managed to write.
func (c *child) kill() []string {
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	for l := range c.lines {
		c.seen = append(c.seen, l)
	}
	return c.seen
}

func has(lines []string, prefix string) (string, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l, true
		}
	}
	return "", false
}
