package ptyholder

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Run is the holder process: it creates the PTY, starts the command, listens on
// the session socket and serves adoptions until released or its linger ends.
// ready receives "ok\n" once the socket is listening and Meta is written (or
// "err: <reason>\n"), then is closed.
func Run(specPath string, ready *os.File) error {
	if ready != nil {
		unix.CloseOnExec(int(ready.Fd()))
	}
	err := run(specPath, ready)
	if err != nil && ready != nil {
		fmt.Fprintf(ready, "err: %v\n", err)
		ready.Close()
	}
	return err
}

type holder struct {
	spec   Spec
	master *os.File
	cmd    *exec.Cmd
	meta   Meta

	exited chan struct{}
	status reply // valid once exited is closed

	audit sync.Mutex
	stop  chan struct{}
	once  sync.Once
}

func run(specPath string, ready *os.File) error {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	_ = os.Remove(specPath) // Env may hold secrets: it is never left on disk
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	if spec.ID == "" || len(spec.Argv) == 0 || spec.Credential == "" || spec.Socket == "" {
		return errors.New("ptyholder: incomplete spec")
	}
	if spec.Cols == 0 {
		spec.Cols = 80
	}
	if spec.Rows == 0 {
		spec.Rows = 24
	}
	if spec.ExitedLinger <= 0 {
		spec.ExitedLinger = 24 * time.Hour
	}
	// Install before spawning: cancellation during the readiness handshake must
	// run child cleanup too. Production holders still ignore HUP/INT below.
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	defer signal.Stop(terms)
	master, tty, err := pty.Open()
	if err != nil {
		return err
	}
	defer master.Close()
	defer tty.Close()
	if err := pty.Setsize(master, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows}); err != nil {
		return err
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Env = spec.Cwd, spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return err
	}
	tty.Close()
	// Survive the terminal and the daemon going away; an interrupted holder is
	// still a holder. Done AFTER the child started: an ignored signal stays
	// ignored across exec, and the shell must keep default SIGINT/SIGHUP/SIGPIPE.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE, syscall.SIGINT)
	h := &holder{spec: spec, master: master, cmd: cmd, exited: make(chan struct{}), stop: make(chan struct{})}
	go h.reap()
	defer func() { h.shutdown(); h.cleanup() }()
	go func() {
		select {
		case <-terms:
			h.shutdown()
		case <-h.stop:
		}
	}()
	if spec.TestOwner != nil {
		go func() {
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				if !spec.TestOwner.alive() {
					h.shutdown()
					return
				}
				select {
				case <-tick.C:
				case <-h.stop:
					return
				}
			}
		}()
	}

	hs, err := StartTime(os.Getpid())
	if err != nil {
		return err
	}
	cs, err := StartTime(cmd.Process.Pid)
	if err != nil {
		cs = 0 // already gone: the adopter sees Exited in the reply
	}
	h.meta = Meta{TestOwner: spec.TestOwner, Proto: ProtoVersion, ID: spec.ID, HolderPID: os.Getpid(), HolderStart: hs,
		ChildPID: cmd.Process.Pid, ChildStart: cs, Socket: spec.Socket}

	if err := os.MkdirAll(filepath.Dir(spec.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(spec.Socket)
	old := unix.Umask(0o177) // the socket is created 0600, never briefly wider
	ln, err := net.Listen("unix", spec.Socket)
	unix.Umask(old)
	if err != nil {
		return err
	}
	_ = os.Chmod(spec.Socket, 0o600)
	if err := writeMeta(spec.Root, h.meta); err != nil {
		ln.Close()
		return err
	}
	if ready != nil {
		fmt.Fprintln(ready, "ok")
		ready.Close()
	}
	go func() {
		<-h.stop
		ln.Close()
	}()
	go h.lingerAfterExit()
	var connMu sync.Mutex
	conns := make(map[*net.UnixConn]struct{})
	var handlers sync.WaitGroup
	slots := make(chan struct{}, 64)
	defer func() {
		_ = ln.Close()
		connMu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		connMu.Unlock()
		handlers.Wait()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-h.stop:
				return nil
			default:
				return err
			}
		}
		uc := c.(*net.UnixConn)
		select {
		case slots <- struct{}{}:
		default:
			_ = uc.Close()
			continue
		}
		connMu.Lock()
		conns[uc] = struct{}{}
		connMu.Unlock()
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { connMu.Lock(); delete(conns, uc); connMu.Unlock(); <-slots }()
			h.serve(uc)
		}()
	}
}

func (h *holder) reap() {
	_ = h.cmd.Wait()
	st := h.cmd.ProcessState
	r := reply{OK: true, Exited: true}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		r.Signal = unix.SignalName(ws.Signal())
	} else {
		r.Code = st.ExitCode()
	}
	h.status = r
	close(h.exited)
}

// lingerAfterExit ends the holder when its child is gone and nobody collected
// the status within the linger: nothing is left for it to keep alive.
func (h *holder) lingerAfterExit() {
	<-h.exited
	select {
	case <-time.After(h.spec.ExitedLinger):
		h.shutdown()
	case <-h.stop:
	}
}

func (h *holder) shutdown() { h.once.Do(func() { close(h.stop) }) }

func (h *holder) cleanup() {
	// Signal only the live, recorded PTY leader's group. Once it has exited,
	// closing the master sends terminal hangup; never sweep a recycled PGID.
	signalChild := func(sig syscall.Signal) {
		select {
		case <-h.exited:
			return
		default:
		}
		if h.meta.ChildStart == 0 {
			// Initialization failed before identity capture: the exec.Cmd owns the
			// leader, but there is no evidence permitting a descendant/group signal.
			_ = h.cmd.Process.Signal(sig)
			return
		}
		st, err := StartTime(h.cmd.Process.Pid)
		if err != nil || st != h.meta.ChildStart {
			return
		}
		_ = syscall.Kill(-h.cmd.Process.Pid, sig)
	}
	signalChild(syscall.SIGTERM)
	_ = h.master.Close()
	select {
	case <-h.exited:
	case <-time.After(200 * time.Millisecond):
		signalChild(syscall.SIGKILL)
		select {
		case <-h.exited:
		case <-time.After(time.Second):
			slog.Error("ptyholder: child reap timeout", "session", h.spec.ID, "pid", h.cmd.Process.Pid)
			if h.meta.ID != "" {
				h.meta.CleanupError = "child reap timeout"
				_ = writeMeta(h.spec.Root, h.meta)
			}
			return // keep identity evidence; Release will report cleanup failure
		}
	}
	_ = os.Remove(h.spec.Socket)
	_ = os.Remove(metaPath(h.spec.Root, h.spec.ID))
}

func (h *holder) refuse(c *net.UnixConn, why string, uid uint32) {
	h.audit.Lock()
	defer h.audit.Unlock()
	if f, err := os.OpenFile(auditPath(h.spec.Root, h.spec.ID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s refused %s peer_uid=%d\n", time.Now().UTC().Format(time.RFC3339Nano), why, uid)
		f.Close()
	}
	writeReply(c, reply{Err: why})
}

func writeReply(c net.Conn, r reply) {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	b, _ := json.Marshal(r)
	_, _ = c.Write(append(b, '\n'))
}

func (h *holder) serve(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	sc, err := c.SyscallConn()
	if err != nil {
		return
	}
	var uid uint32
	var perr error
	_ = sc.Control(func(fd uintptr) { uid, _, perr = peer(int(fd)) })
	if perr != nil || int(uid) != os.Getuid() {
		h.refuse(c, "peer uid not allowed", uid)
		return
	}
	line, err := bufio.NewReaderSize(c, 4096).ReadSlice('\n')
	if err != nil {
		return
	}
	var req request
	if json.Unmarshal(line, &req) != nil {
		h.refuse(c, "malformed request", uid)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.Proto != ProtoVersion {
		h.refuse(c, fmt.Sprintf("protocol %d unsupported (holder speaks %d)", req.Proto, ProtoVersion), uid)
		return
	}
	if req.ID != h.spec.ID || subtle.ConstantTimeCompare([]byte(req.Cred), []byte(h.spec.Credential)) != 1 {
		h.refuse(c, "bad credential", uid)
		return
	}
	switch req.Op {
	case "adopt":
		r := reply{OK: true, Proto: ProtoVersion, PID: h.meta.ChildPID, Start: h.meta.ChildStart}
		select {
		case <-h.exited:
			r.Exited, r.Code, r.Signal = true, h.status.Code, h.status.Signal
		default:
		}
		b, _ := json.Marshal(r)
		var werr error
		_ = h.sendMaster(sc, c, append(b, '\n'), &werr)
	case "wait":
		disconnected := make(chan struct{})
		go func() { var b [1]byte; _, _ = c.Read(b[:]); close(disconnected) }()
		select {
		case <-disconnected:
			return
		case <-h.exited:
			r := h.status
			r.Proto = ProtoVersion
			writeReply(c, r)
		case <-h.stop:
		}
	case "release":
		writeReply(c, reply{OK: true, Proto: ProtoVersion})
		h.shutdown()
	default:
		h.refuse(c, "unknown op", uid)
	}
}

// sendMaster writes the reply with a dup of the master attached (SCM_RIGHTS).
func (h *holder) sendMaster(_ interface{}, c *net.UnixConn, b []byte, werr *error) error {
	msc, err := h.master.SyscallConn()
	if err != nil {
		return err
	}
	return msc.Control(func(fd uintptr) {
		_, _, *werr = c.WriteMsgUnix(b, unix.UnixRights(int(fd)), nil)
	})
}
