package ptyholder

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyruntime"
)

// LaunchSpec describes a new holder-hosted session.
type LaunchSpec struct {
	// Root is the holder state directory (created 0700).
	Root string
	ID   string
	Argv []string
	Cwd  string
	Env  []string
	Cols uint16
	Rows uint16
	// Command is how to start a holder; the spec path is appended. Default:
	// {os.Executable(), "ptyholder"}.
	Command []string
	// ExitedLinger overrides the holder's post-exit linger (tests).
	ExitedLinger time.Duration
}

// Handle names one holder: where its state lives and the credential for it.
type Handle struct {
	Root string
	ID   string
	cred string
}

// Launch starts a holder (its own session; the daemon dying does not signal
// it) and waits until it is listening. It does NOT adopt: call Adopt.
func Launch(ctx context.Context, ls LaunchSpec) (h *Handle, err error) {
	if !ValidID(ls.ID) {
		return nil, fmt.Errorf("ptyholder: invalid session id %q", ls.ID)
	}
	defer func() {
		if err != nil {
			Forget(ls.Root, ls.ID) // a failed launch leaves no record that looks adoptable
		}
	}()
	if err := ensureDir(ls.Root); err != nil {
		return nil, err
	}
	cred, err := newCredential()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(credPath(ls.Root, ls.ID), []byte(cred)); err != nil {
		return nil, err
	}
	spec := Spec{ID: ls.ID, Root: ls.Root, Argv: ls.Argv, Cwd: ls.Cwd, Env: ls.Env, Cols: ls.Cols, Rows: ls.Rows,
		Credential: cred, Socket: SocketPath(ls.Root, ls.ID), ExitedLinger: ls.ExitedLinger}
	sp := specPath(ls.Root, ls.ID)
	if err := writeFileAtomic(sp, mustJSON(spec)); err != nil {
		return nil, err
	}
	if err := ensureSocketDir(spec.Socket); err != nil {
		return nil, err
	}
	cmdline := ls.Command
	if len(cmdline) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmdline = []string{exe, "ptyholder"}
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	cmd := exec.Command(cmdline[0], append(append([]string{}, cmdline[1:]...), sp)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{wr} // fd 3
	cmd.Env = append(os.Environ(), "WARDEN_PTYHOLDER_READY_FD=3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		wr.Close()
		_ = os.Remove(sp)
		return nil, err
	}
	wr.Close()
	go func() { _ = cmd.Wait() }() // reap while this process lives; afterwards init does
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := bufio.NewReader(rd).ReadString('\n')
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.line != "ok\n" {
			_ = os.Remove(sp)
			return nil, fmt.Errorf("ptyholder: holder failed to start: %q %v", r.line, r.err)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = os.Remove(sp)
		return nil, ctx.Err()
	}
	return &Handle{Root: ls.Root, ID: ls.ID, cred: cred}, nil
}

// Open returns the handle of an existing holder from its persisted credential.
func Open(root, id string) (*Handle, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("ptyholder: invalid session id %q", id)
	}
	if err := checkOwned(credPath(root, id), 0o600); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(credPath(root, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrGone
		}
		return nil, err
	}
	return &Handle{Root: root, ID: id, cred: string(b)}, nil
}

// Exists reports whether a holder record exists for id (it may be dead).
func Exists(root, id string) bool {
	_, err := os.Stat(credPath(root, id))
	return err == nil
}

// Forget removes a session's holder state (after release or a confirmed-dead
// holder).
func Forget(root, id string) {
	for _, p := range []string{metaPath(root, id), credPath(root, id), specPath(root, id), auditPath(root, id), SocketPath(root, id)} {
		_ = os.Remove(p)
	}
}

func newCredential() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkOwned(dir, 0o700)
}

func ensureSocketDir(sock string) error {
	d := filepath.Dir(sock)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return checkOwned(d, 0o700)
}

// checkOwned verifies p is owned by this uid and has no group/other bits.
func checkOwned(p string, max os.FileMode) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is not owned by this user", ErrInsecure, p)
	}
	if fi.Mode().Perm()&^max != 0 || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %v", ErrInsecure, p, fi.Mode().Perm())
	}
	return nil
}

func (h *Handle) meta() (Meta, error) {
	if err := checkOwned(metaPath(h.Root, h.ID), 0o600); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, ErrGone
		}
		return Meta{}, err
	}
	b, err := os.ReadFile(metaPath(h.Root, h.ID))
	if err != nil {
		return Meta{}, ErrGone
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("%w: unreadable meta", ErrIdentity)
	}
	return m, nil
}

// dial connects to the holder socket after checking that the socket is a 0600
// socket owned by this uid and that the listener is the recorded holder
// process (peer pid + start time).
func (h *Handle) dial(ctx context.Context, m Meta) (*net.UnixConn, error) {
	fi, err := os.Lstat(m.Socket)
	if err != nil {
		return nil, ErrGone
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("%w: %s is not a socket", ErrInsecure, m.Socket)
	}
	if err := checkOwned(m.Socket, 0o600); err != nil {
		return nil, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", m.Socket)
	if err != nil {
		return nil, ErrGone
	}
	uc := c.(*net.UnixConn)
	sc, err := uc.SyscallConn()
	if err != nil {
		uc.Close()
		return nil, err
	}
	var uid uint32
	var pid int
	var perr error
	_ = sc.Control(func(fd uintptr) { uid, pid, perr = peer(int(fd)) })
	if perr != nil || int(uid) != os.Getuid() || pid != m.HolderPID {
		uc.Close()
		return nil, fmt.Errorf("%w: socket listener is not the recorded holder", ErrIdentity)
	}
	if st, err := StartTime(pid); err != nil || st != m.HolderStart {
		uc.Close()
		return nil, fmt.Errorf("%w: holder pid %d was recycled", ErrIdentity, pid)
	}
	return uc, nil
}

func (h *Handle) send(c *net.UnixConn, op string) error {
	b, _ := json.Marshal(request{Op: op, Proto: ProtoVersion, ID: h.ID, Cred: h.cred})
	_, err := c.Write(append(b, '\n'))
	return err
}

func readReply(c *net.UnixConn) (reply, []*os.File, error) {
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
		return reply{}, nil, err
	}
	var files []*os.File
	if oobn > 0 {
		if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for _, m := range msgs {
				if fds, ferr := unix.ParseUnixRights(&m); ferr == nil {
					for _, fd := range fds {
						unix.CloseOnExec(fd)
						files = append(files, os.NewFile(uintptr(fd), "ptmx"))
					}
				}
			}
		}
	}
	var r reply
	if err := json.Unmarshal(buf[:n], &r); err != nil {
		for _, f := range files {
			f.Close()
		}
		return reply{}, nil, err
	}
	return r, files, nil
}

func classify(r reply) error {
	if r.OK {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrAuth, r.Err)
}

// Adopt authenticates to the holder and returns the live process as an
// ptyruntime.Adoption (this process's own dup of the master, the exit-status
// wait, an identity-checked liveness probe). A recycled pid, a foreign holder,
// a wrong credential or uid, or a protocol mismatch is refused.
func (h *Handle) Adopt(ctx context.Context, recovered bool) (*ptyruntime.Adoption, error) {
	m, err := h.meta()
	if err != nil {
		return nil, err
	}
	if m.Proto != ProtoVersion {
		return nil, fmt.Errorf("%w: holder speaks %d, client %d", ErrProto, m.Proto, ProtoVersion)
	}
	c, err := h.dial(ctx, m)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := h.send(c, "adopt"); err != nil {
		return nil, ErrGone
	}
	r, files, err := readReply(c)
	if err != nil {
		return nil, ErrGone
	}
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	if err := classify(r); err != nil {
		closeAll()
		return nil, err
	}
	if r.Proto != ProtoVersion {
		closeAll()
		return nil, ErrProto
	}
	if len(files) != 1 {
		closeAll()
		return nil, fmt.Errorf("%w: holder sent %d descriptors", ErrIdentity, len(files))
	}
	if r.PID != m.ChildPID || r.Start != m.ChildStart {
		closeAll()
		return nil, fmt.Errorf("%w: child (%d,%d) is not the recorded (%d,%d)", ErrIdentity, r.PID, r.Start, m.ChildPID, m.ChildStart)
	}
	if !r.Exited && m.ChildStart != 0 {
		// A child that has already exited is simply gone (the holder reports its
		// status); only a LIVE process with a different start time is a recycled pid.
		if st, err := StartTime(r.PID); err == nil && st != m.ChildStart {
			closeAll()
			return nil, fmt.Errorf("%w: child pid %d was recycled", ErrIdentity, r.PID)
		}
	}
	master := files[0]
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		closeAll()
		return nil, err
	}
	// os.NewFile on a non-blocking fd registers it with the poller only when it
	// was non-blocking at creation; re-wrap so Close interrupts a pending Read.
	fd, err := unix.Dup(int(master.Fd()))
	master.Close()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	_ = unix.SetNonblock(fd, true)
	master = os.NewFile(uintptr(fd), "ptmx")

	ad := &ptyruntime.Adoption{Master: master, PID: r.PID, Recovered: recovered}
	var (
		mu   sync.Mutex
		wc   *net.UnixConn
		done bool
	)
	pre := r
	ad.Wait = func() ptyruntime.ExitStatus {
		if pre.Exited {
			return ptyruntime.ExitStatus{Code: pre.Code, Signal: pre.Signal}
		}
		c, err := h.dial(context.Background(), m)
		if err != nil {
			return ptyruntime.ExitStatus{Lost: true}
		}
		mu.Lock()
		if done {
			mu.Unlock()
			c.Close()
			return ptyruntime.ExitStatus{Lost: true}
		}
		wc = c
		mu.Unlock()
		defer c.Close()
		if h.send(c, "wait") != nil {
			return ptyruntime.ExitStatus{Lost: true}
		}
		wr, _, err := readReply(c)
		if err != nil || !wr.OK || !wr.Exited {
			return ptyruntime.ExitStatus{Lost: true}
		}
		return ptyruntime.ExitStatus{Code: wr.Code, Signal: wr.Signal}
	}
	ad.Abort = func() {
		mu.Lock()
		done = true
		if wc != nil {
			wc.Close()
		}
		mu.Unlock()
	}
	ad.Alive = func() bool {
		st, err := StartTime(r.PID)
		return err == nil && (m.ChildStart == 0 || st == m.ChildStart)
	}
	return ad, nil
}

// Release tells the holder the session is over for good: it closes its master
// and exits. Call it after the process has been killed or has exited.
func (h *Handle) Release(ctx context.Context) error {
	m, err := h.meta()
	if err != nil {
		return err
	}
	c, err := h.dial(ctx, m)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := h.send(c, "release"); err != nil {
		return ErrGone
	}
	r, _, err := readReply(c)
	if err != nil {
		return ErrGone
	}
	return classify(r)
}

// Snapshot of what a holder record says without touching the socket (for
// diagnostics): ok is false when the record is unreadable.
func (h *Handle) Meta() (Meta, bool) {
	m, err := h.meta()
	return m, err == nil
}
