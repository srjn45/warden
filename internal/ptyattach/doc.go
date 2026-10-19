// Package ptyattach is the local terminal client of the §6 PTY transport
// (plan 02 t3, gaps G9/G10): it attaches the user's own terminal to a
// warden-owned PTY session — a direct plain terminal — so `wd attach` and the
// cockpit's terminal pane can show and drive it without tmux.
//
// The client puts the local tty in raw mode (so Ctrl-C, Ctrl-D, Ctrl-Z and every
// escape sequence travel to the session's PTY as bytes, where the REMOTE line
// discipline turns them into SIGINT/EOF/SIGTSTP for the foreground job — job
// control and process groups are the PTY's, not ours), renders the replay and
// live feed in order, forwards keystrokes as fenced input frames tagged with the
// controller generation and a per-client input id, follows SIGWINCH, and on any
// exit path — detach, process exit, error, SIGTERM/SIGHUP — restores the
// terminal modes it changed.
//
// Local rendering is faithful: the raw live bytes go to the user's terminal
// unredacted. Only replay, which comes from the journal, is redacted.
package ptyattach
