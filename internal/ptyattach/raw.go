//go:build linux || darwin

package ptyattach

import (
	"os"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGet)
	return err == nil
}

// MakeRaw puts the terminal on f into raw mode and returns the function that
// restores the exact previous settings. Raw means no echo, no canonical
// buffering and no signal generation: Ctrl-C/Ctrl-\/Ctrl-Z reach the remote PTY
// as plain bytes instead of signalling this process.
func MakeRaw(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGet)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSet, &raw); err != nil {
		return nil, err
	}
	saved := *old
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSet, &saved) }, nil
}

// Size returns the terminal's window size.
func Size(f *os.File) (cols, rows uint16, err error) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return ws.Col, ws.Row, nil
}
