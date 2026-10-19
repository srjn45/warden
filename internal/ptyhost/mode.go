package ptyhost

import "strings"

// Mode is the `runtime.pty` setting.
type Mode string

const (
	// ModeOff keeps every session on the legacy tmux path with no journal.
	ModeOff Mode = "off"
	// ModeShadow mirrors tmux panes into journals (M1); nothing is written.
	ModeShadow Mode = "shadow"
	// ModeOn additionally hosts plain-shell terminals on a direct PTY.
	ModeOn Mode = "on"
)

// ParseMode normalizes a config value. Anything unrecognized is off.
func ParseMode(s string) Mode {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case ModeShadow, ModeOn:
		return m
	}
	return ModeOff
}

// Valid reports whether s names a mode exactly (case-insensitive).
func Valid(s string) bool {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeOff, ModeShadow, ModeOn:
		return true
	}
	return false
}

// mirrors reports whether tmux panes are journaled in this mode.
func (m Mode) mirrors() bool { return m == ModeShadow || m == ModeOn }
