package tui

import (
	"errors"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/client"
)

// fleetStatus describes the health of the most recent fleet poll. It drives the
// control pane's last-known-good behavior: on anything other than fleetLive the
// pane keeps rendering the previous complete snapshot (rows, selection, layout)
// and surfaces a persistent, non-blocking banner that says why the data may be
// stale. Rows are never cleared on a failed poll — an agent is only dropped when
// a later *complete* snapshot omits it.
type fleetStatus int

const (
	// fleetLive is the zero value: the last poll returned a complete, authoritative
	// fleet snapshot, so the displayed rows are current.
	fleetLive fleetStatus = iota
	// fleetDisconnected: the daemon is unreachable (connection refused). The last
	// complete snapshot is retained.
	fleetDisconnected
	// fleetTimeout: the request did not complete cleanly — our per-call deadline
	// elapsed or the transport failed without a status. The daemon may be slow or
	// wedged rather than down, so we keep last-known-good and say so.
	fleetTimeout
	// fleetDegraded: the daemon answered but its active session store is degraded
	// (HTTP 503 under the complete-or-error contract). No authoritative fleet was
	// returned, so the last complete snapshot is retained rather than a partial one.
	fleetDegraded
)

// classifyFleetErr maps a List error onto a fleetStatus so the banner can tell a
// dead daemon, a timed-out request, and a degraded store apart. A nil error is
// fleetLive. Any reachable-but-erroring daemon that is not a 503 is treated as a
// timeout/"not responding" rather than "down", so we never mislabel a live daemon
// as stopped.
func classifyFleetErr(err error) fleetStatus {
	if err == nil {
		return fleetLive
	}
	if errors.Is(err, client.ErrDaemonDown) {
		return fleetDisconnected
	}
	var se *client.StatusError
	if errors.As(err, &se) && se.Code == http.StatusServiceUnavailable {
		return fleetDegraded
	}
	// Our own bg() deadline, or any other transport error, means the daemon did
	// not answer cleanly but isn't provably down.
	return fleetTimeout
}

// fleetIndicator renders the short header status chip for a fleet state.
func fleetIndicator(f fleetStatus) string {
	switch f {
	case fleetLive:
		return stStatus.Render("live ●")
	case fleetDegraded:
		return stAttention.Render("degraded ●")
	case fleetTimeout:
		return stAttention.Render("stale ●")
	default: // fleetDisconnected
		return stError.Render("reconnecting…")
	}
}

// fleetBannerDetail returns the persistent, non-blocking banner line for a
// degraded/disconnected/stale fleet, or "" when live. When a prior complete
// snapshot exists its wall-clock is appended ("showing last complete fleet from
// HH:MM:SS") so the operator knows the rows are retained but may be stale. The
// banner intentionally carries no per-record store detail — that can include
// session ids and paths and is available via the store-health endpoint instead.
func fleetBannerDetail(f fleetStatus, lastCompleteAt time.Time) string {
	if f == fleetLive {
		return ""
	}
	stamp := ""
	if !lastCompleteAt.IsZero() {
		stamp = " · showing last complete fleet from " + lastCompleteAt.Format("15:04:05")
	}
	switch f {
	case fleetDegraded:
		return "session store degraded" + stamp
	case fleetTimeout:
		return "daemon not responding — request timed out" + stamp
	default: // fleetDisconnected
		if stamp == "" {
			// Never connected this session: the actionable hint is the whole message.
			return "daemon not running — start it with `warden daemon`"
		}
		return "daemon not running — start it with `warden daemon`" + stamp
	}
}

// fleetBannerStyled applies the severity color to a banner detail line: red for a
// dead daemon, amber for a stale/degraded-but-reachable daemon.
func fleetBannerStyled(f fleetStatus, lastCompleteAt time.Time) string {
	detail := fleetBannerDetail(f, lastCompleteAt)
	if detail == "" {
		return ""
	}
	if f == fleetDisconnected {
		return stError.Render(detail)
	}
	return stAttention.Render(detail)
}
