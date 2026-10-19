//go:build linux || darwin

package ptyruntime

import (
	"os"

	"golang.org/x/sys/unix"

	"github.com/srjn45/warden/internal/ptyevent"
)

// ttyMode reads the slave's line discipline through the master. Linux routes
// TCGETS on a pty master to the slave's termios; BSD/macOS share one termios
// for the pair. It goes through SyscallConn so the master stays non-blocking
// (os.File.Fd would flip it to blocking and Close could no longer interrupt a
// pending Read).
func ttyMode(f *os.File) (ptyevent.TTYMode, error) {
	var t *unix.Termios
	err := control(f, func(fd int) (err error) {
		t, err = unix.IoctlGetTermios(fd, ioctlGetTermios)
		return err
	})
	if err != nil {
		return ptyevent.TTYMode{}, err
	}
	return ptyevent.TTYMode{Canonical: t.Lflag&unix.ICANON != 0, Echo: t.Lflag&unix.ECHO != 0}, nil
}

// setWinsize applies a window size to the PTY (TIOCSWINSZ, which also sends
// SIGWINCH to the foreground process group).
func setWinsize(f *os.File, cols, rows uint16) error {
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
}

func control(f *os.File, fn func(fd int) error) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := sc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}
