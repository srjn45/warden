package daemon

import (
	"errors"
	"sort"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// parked reports whether a recipient is safe to wake with an injected notice.
// A working/spawning agent is NEVER interrupted (the message waits in the inbox).
func parked(st store.Status) bool {
	return st == store.StatusIdle || st == store.StatusWaitingForInput
}

// reservedSenders are provenance ids only the daemon itself may stamp. A caller
// (agent or human) that supplies one is rejected by sanitizeSender, so automated
// daemon-originated provenance (e.g. a "daemon" conflict warning) can't be forged
// — the validation half of warden's "from/updated_by is advisory, not an
// authenticated identity" trust model. "human" is deliberately NOT reserved:
// it's the default identity for human-originated writes.
var reservedSenders = map[string]bool{"daemon": true, "system": true}

// errReservedSender is returned when a caller supplies a reserved provenance id.
var errReservedSender = errors.New("sender id is reserved for the daemon")

// sanitizeSender is the single write gate behind every agent-reachable write
// path (messages and context): it rejects reserved ids and applies the "human"
// default for an empty id. Daemon-internal writes call the stores directly and
// are trusted by construction, so they bypass this gate.
func sanitizeSender(from string) (string, error) {
	if reservedSenders[from] {
		return "", errReservedSender
	}
	if from == "" {
		from = "human"
	}
	return from, nil
}

// defaultRecentLimit caps GET /messages when the caller gives no (or a
// non-positive) limit — enough to fill an inspector view without unbounded reads.
const defaultRecentLimit = 50

// recentMessages returns msgs newest-first, capped to limit (defaultRecentLimit
// when limit <= 0). Pure: it copies before sorting so the caller's slice is
// untouched. Always returns a non-nil slice.
func recentMessages(msgs []mailbox.Message, limit int) []mailbox.Message {
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	out := append([]mailbox.Message{}, msgs...)
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
