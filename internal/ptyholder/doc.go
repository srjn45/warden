// Package ptyholder is the independent owner of a PTY master (plan 02 t3,
// gaps G1–G3 of docs/specs/2026-10-19-plain-terminal-inventory-and-contract.md).
//
// Plan 01 left the warden daemon holding every direct session's master: when the
// daemon exited the master closed and the shell received SIGHUP, and "recovery"
// started a brand-new shell. A holder is a tiny per-session process (the same
// binary re-exec'd, started in its own session) that creates the PTY, starts the
// command as its child and keeps the master and the wait status. The daemon is a
// client:
//
//   - the holder hands the daemon its OWN dup of the master over a Unix socket
//     (SCM_RIGHTS), so the daemon reads/writes the PTY directly (the existing
//     ptyruntime reader, journal and transport are reused unchanged) while the
//     holder's copy keeps the master — and therefore the process tree — alive
//     through a daemon restart, update or crash;
//   - the daemon dies or detaches: nothing happens to the process. It restarts,
//     authenticates and is handed the master again (an adoption);
//   - the holder reaps the leader and reports its exit status, so a shell that
//     ended while the daemon was away is journaled with its real status.
//
// # Process that holds the master
//
// Holder-hosted session: the holder process AND the daemon each hold a master
// descriptor; the child's controlling terminal is the slave. The daemon's copy
// is disposable. tmux-hosted sessions (every agent, and every session while
// runtime.pty is off/shadow) keep the tmux server as the holder; it has no
// warden credential or fencing, which is why agents are adopted through the
// tmux-compat identity checks (see ptyhost) instead.
//
// # Authentication
//
// The socket lives in a 0700 directory and is 0600. The holder checks the peer
// uid (SO_PEERCRED / LOCAL_PEERCRED) and a per-session random credential
// (constant-time compare) persisted 0600 beside the journal. The daemon in turn
// checks the socket owner/mode, that the peer is the recorded holder pid with
// the recorded process start time, and that the child it is handed has the
// recorded (pid, start time): a recycled pid, a foreign process or another uid
// is never adopted. Refusals are appended to <id>.audit (reason, peer uid, time
// — never the credential or any terminal bytes).
package ptyholder
