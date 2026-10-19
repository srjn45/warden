// Package ptyhost wires the warden-owned PTY runtime (internal/ptyruntime)
// into session lifecycle behind `runtime.pty: off|shadow|on` (t2-2 of the PTY
// runtime contract, docs/specs/2026-10-19-pty-runtime-contract.md §12 M1).
//
//   - off (default): nothing changes. Every pane is a plain tmux session and
//     no journal is written.
//   - shadow: panes stay in tmux exactly as today (same launch path), and
//     each new pane's output is mirrored into a per-session journal through
//     `tmux pipe-pane -O`. The mirror is observe-only: warden writes nothing
//     to the pane through it.
//   - on: as shadow for agent panes; additionally plain-shell terminals
//     (kind=terminal, the one provider-neutral lifecycle path migrated here)
//     are hosted directly on a warden-owned PTY — launch, stream, resize,
//     teardown and restart recovery through journal epochs.
//
// Manager is the single entry point. Its Host routes the tmuxproc.Host seam
// (create/probe/capture/key/kill) to the direct runtime or tmux by session
// name; Lookup hands the §6 transport a journaled stream; observers receive
// every journaled event so status/approvals can consume the stream without
// replacing the legacy pollers (t3 makes the classifier authoritative).
package ptyhost
