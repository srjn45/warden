package ptyhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyholder"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/tmuxproc"
)

// Shadow mirrors tmux panes into journals (M1). Each mirrored pane gets a
// FIFO that `tmux pipe-pane -O` copies its output into, and a
// ptyruntime.Mirror reading the other end. Shadow writes nothing to a pane.
//
// The daemon holds a second ("keeper") write end of every FIFO, so a read
// never sees EOF just because the pipe-pane cat has not connected yet or was
// restarted. Stop closes the pipe-pane and the keeper; only then does the
// mirror drain to EOF.
type Shadow struct {
	run tmuxproc.Runner
	// dir holds one journal per pane name, plus FIFOs under dir/.fifo.
	dir string
	ret ptyruntime.Retention
	opt ptyruntime.Options
	// stopWait bounds how long Stop waits for a pane's pipe to drain.
	stopWait time.Duration

	mu      sync.Mutex
	mirrors map[string]*mirrored
}

type mirrored struct {
	m      *ptyruntime.Mirror
	j      *ptyruntime.Journal
	src    *os.File
	keeper *os.File
	fifo   string
}

func newShadow(run tmuxproc.Runner, dir string, ret ptyruntime.Retention, opt ptyruntime.Options) *Shadow {
	return &Shadow{run: run, dir: dir, ret: ret, opt: opt, stopWait: 5 * time.Second, mirrors: map[string]*mirrored{}}
}

func (sh *Shadow) journalDir(name string) string { return filepath.Join(sh.dir, name) }

// HasJournal reports whether name has been mirrored before (a journal
// exists), which is what makes a live pane eligible for re-adoption.
func (sh *Shadow) HasJournal(name string) bool {
	fi, err := os.Stat(sh.journalDir(name))
	return err == nil && fi.IsDir()
}

// Mirror returns the live mirror for name (nil if none).
func (sh *Shadow) Mirror(name string) *ptyruntime.Mirror {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if mr, ok := sh.mirrors[name]; ok {
		return mr.m
	}
	return nil
}

// Names lists the panes currently mirrored.
func (sh *Shadow) Names() []string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := make([]string, 0, len(sh.mirrors))
	for n := range sh.mirrors {
		out = append(out, n)
	}
	return out
}

// Start mirrors the tmux pane name. With recover the pane outlived a previous
// mirror (daemon restart): the epoch opens with lifecycle{recovered} and the
// screen model is seeded from capture-pane. A stale mirror under the same name
// (its pane was replaced) is ended first.
func (sh *Shadow) Start(ctx context.Context, name string, recover bool) (*ptyruntime.Mirror, error) {
	if strings.ContainsAny(name, `/\`) || name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("ptyhost: invalid session name %q", name)
	}
	if old := sh.take(name); old != nil {
		sh.stop(ctx, name, old, &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, Detail: "pane replaced"})
	}
	j, err := ptyruntime.OpenJournal(sh.journalDir(name), name, sh.ret)
	if err != nil {
		return nil, err
	}
	src, keeper, fifo, err := sh.openFIFO(name)
	if err != nil {
		_ = j.Close()
		return nil, err
	}
	cfg := ptyruntime.MirrorConfig{Journal: j, Source: src, Recover: recover, Options: sh.opt}
	cfg.Cols, cfg.Rows = sh.paneSize(ctx, name)
	if recover {
		cfg.Seed, _ = sh.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", name)
	}
	m, err := ptyruntime.StartMirror(cfg)
	if err != nil {
		_ = keeper.Close()
		_ = src.Close()
		_ = os.Remove(fifo)
		_ = j.Close()
		return nil, err
	}
	mr := &mirrored{m: m, j: j, src: src, keeper: keeper, fifo: fifo}
	if out, err := sh.run.Run(ctx, "", "tmux", "pipe-pane", "-O", "-t", name, "exec cat >> "+shellQuote(fifo)); err != nil {
		sh.stop(ctx, name, mr, nil)
		return nil, fmt.Errorf("tmux pipe-pane: %w: %s", err, out)
	}
	sh.mu.Lock()
	sh.mirrors[name] = mr
	sh.mu.Unlock()
	if !recover {
		sh.recordPane(ctx, name)
	}
	return m, nil
}

// The tmux server is the process that holds a mirrored pane's PTY master (the
// "tmux-compat holder": agents are never moved onto a warden holder). It has no
// warden credential, but the pane can still be identified: its process (pid and
// start time) is recorded when the mirror starts and checked before a restart
// re-adopts it, so a pane that was replaced by an unrelated process under the
// same name is never claimed as the old agent. The tmux socket itself is
// private to this uid (tmux creates its directory 0700).

const paneIdentityFile = "pane.json"

type paneIdentity struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

func (sh *Shadow) panePID(ctx context.Context, name string) (int, bool) {
	out, err := sh.run.Run(ctx, "", "tmux", "display-message", "-p", "-t", name, "#{pane_pid}")
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	return pid, err == nil && pid > 0
}

func (sh *Shadow) recordPane(ctx context.Context, name string) {
	pid, ok := sh.panePID(ctx, name)
	if !ok {
		return
	}
	start, err := ptyholder.StartTime(pid)
	if err != nil {
		return
	}
	b, _ := json.Marshal(paneIdentity{PID: pid, Start: start})
	_ = os.WriteFile(filepath.Join(sh.journalDir(name), paneIdentityFile), b, 0o600)
}

// VerifyPane checks that the live pane under name is the process that was
// mirrored. A pane with no recorded identity (mirrored before this check
// existed) is accepted; a different or recycled process is refused with the
// reason.
func (sh *Shadow) VerifyPane(ctx context.Context, name string) error {
	b, err := os.ReadFile(filepath.Join(sh.journalDir(name), paneIdentityFile))
	if err != nil {
		return nil
	}
	var want paneIdentity
	if json.Unmarshal(b, &want) != nil {
		return fmt.Errorf("pane identity record for %s is unreadable", name)
	}
	pid, ok := sh.panePID(ctx, name)
	if !ok {
		return fmt.Errorf("tmux pane %s has no process", name)
	}
	start, err := ptyholder.StartTime(pid)
	if pid != want.PID || err != nil || start != want.Start {
		return fmt.Errorf("tmux pane %s is now pid %d, not the mirrored pid %d (start time differs): not adopting a different process", name, pid, want.PID)
	}
	return nil
}

// openFIFO (re)creates name's FIFO and opens the mirror's read end plus the
// keeper write end. Both opens are non-blocking, so neither waits for tmux.
func (sh *Shadow) openFIFO(name string) (src, keeper *os.File, path string, err error) {
	dir := filepath.Join(sh.dir, ".fifo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, "", err
	}
	path = filepath.Join(dir, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, "", err
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, nil, "", fmt.Errorf("mkfifo %s: %w", path, err)
	}
	rfd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(path)
		return nil, nil, "", err
	}
	kfd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(rfd)
		_ = os.Remove(path)
		return nil, nil, "", err
	}
	if !fifoPollable {
		if err := unix.SetNonblock(rfd, false); err != nil {
			_ = unix.Close(rfd)
			_ = unix.Close(kfd)
			_ = os.Remove(path)
			return nil, nil, "", err
		}
	}
	return os.NewFile(uintptr(rfd), path), os.NewFile(uintptr(kfd), path+" (keeper)"), path, nil
}

// paneSize reads the pane's dimensions (0, 0 when unknown → runtime default).
func (sh *Shadow) paneSize(ctx context.Context, name string) (cols, rows uint16) {
	out, err := sh.run.Run(ctx, "", "tmux", "display-message", "-p", "-t", name, "#{pane_width} #{pane_height}")
	if err != nil {
		return 0, 0
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0
	}
	c, err1 := strconv.ParseUint(f[0], 10, 16)
	r, err2 := strconv.ParseUint(f[1], 10, 16)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return uint16(c), uint16(r)
}

func (sh *Shadow) take(name string) *mirrored {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	mr := sh.mirrors[name]
	delete(sh.mirrors, name)
	return mr
}

// Stop ends the mirror of name. A non-nil exit is journaled as the end of the
// epoch (the pane is gone); nil leaves the epoch open for re-adoption (the
// pane lives on, e.g. across a daemon restart).
func (sh *Shadow) Stop(ctx context.Context, name string, exit *ptyevent.Lifecycle) {
	if mr := sh.take(name); mr != nil {
		sh.stop(ctx, name, mr, exit)
	}
}

func (sh *Shadow) stop(ctx context.Context, name string, mr *mirrored, exit *ptyevent.Lifecycle) {
	// Closing the pane's pipe ends the cat; closing the keeper then lets the
	// mirror read EOF once everything the cat wrote is drained.
	_, _ = sh.run.Run(ctx, "", "tmux", "pipe-pane", "-t", name)
	_ = mr.keeper.Close()
	t := time.NewTimer(sh.stopWait)
	select {
	case <-mr.m.Done():
	case <-t.C:
		slog.Warn("ptyhost: shadow pipe did not drain; closing it", "session", name)
		_ = mr.src.Close() // interrupts the read where the FIFO is pollable
	}
	t.Stop()
	if exit != nil {
		if err := mr.m.Exit(*exit); err != nil {
			slog.Warn("ptyhost: journal shadow exit failed", "session", name, "err", err)
		}
	}
	select {
	case <-mr.m.Done():
		_ = mr.m.Close()
	default:
		// Non-pollable FIFO still held by a straggling cat: its read ends when
		// that writer goes away. Close in the background so Stop never hangs.
		go func() { _ = mr.m.Close(); _ = mr.j.Close() }()
		_ = os.Remove(mr.fifo)
		return
	}
	_ = mr.j.Close()
	_ = os.Remove(mr.fifo)
}

// Reconcile ends the mirror of every pane whose tmux session is gone.
func (sh *Shadow) Reconcile(ctx context.Context, alive func(name string) bool) {
	for _, name := range sh.Names() {
		if !alive(name) {
			sh.Stop(ctx, name, &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, Detail: "tmux session ended"})
		}
	}
}

// Close stops every mirror, leaving each epoch open for re-adoption.
func (sh *Shadow) Close(ctx context.Context) {
	for _, name := range sh.Names() {
		sh.Stop(ctx, name, nil)
	}
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
