package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// sendMessageRequest is the body for POST /sessions/{id}/messages.
type sendMessageRequest struct {
	From string `json:"from"` // "" -> "human"
	Body string `json:"body"`
}

// sendMessageResponse is the body for POST /sessions/{id}/messages.
type sendMessageResponse struct {
	Message mailbox.Message `json:"message"`
	Woke    bool            `json:"woke"`
}

// inboxResponse is the body for GET /sessions/{id}/messages.
type inboxResponse struct {
	Messages []mailbox.Message `json:"messages"`
}

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

// recentMessagesResponse is the body for GET /messages.
type recentMessagesResponse struct {
	Messages []mailbox.Message `json:"messages"`
}

func (s *Server) registerMessageRoutes(r chi.Router) {
	r.Post("/sessions/{id}/messages", s.handleSendMessage)
	r.Get("/sessions/{id}/messages", s.handleInbox)
	r.Get("/sessions/{id}/messages/wait", s.handleWaitMessage)
	r.Get("/messages", s.handleRecentMessages)
}

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

// handleRecentMessages serves a global, READ-ONLY view of recent message traffic
// across every agent inbox. Unlike GET /sessions/{id}/messages, it never marks
// anything read — it backs the inspector, not message consumption.
func (s *Server) handleRecentMessages(w http.ResponseWriter, r *http.Request) {
	limit := defaultRecentLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	all, err := s.mbox.All()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recentMessagesResponse{Messages: recentMessages(all, limit)})
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Existence check up front: reject a send to an unknown session before
	// touching the inbox. Status is re-read later, just before the wake.
	if _, err := s.store.Get(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Body == "" {
		writeErr(w, http.StatusBadRequest, "empty message body")
		return
	}
	from, err := sanitizeSender(req.From)
	if errors.Is(err, errReservedSender) {
		writeErr(w, http.StatusForbidden, "sender id is reserved for the daemon")
		return
	}
	m, err := s.mbox.Append(mailbox.Message{To: id, From: from, Body: req.Body})
	if errors.Is(err, mailbox.ErrBadRecipient) {
		writeErr(w, http.StatusBadRequest, "invalid recipient")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Waking is best-effort and the inbox above is the source of truth: the
	// message is already durably appended regardless of what happens here. We
	// re-Get status as close to the injection as possible to shrink the
	// idle→working TOCTOU (the earlier Get above can be stale by now), and accept
	// that Input may still race a transition. Do NOT turn this into a lock — that
	// would serialize sends against the poller for a pure optimization.
	woke := false
	if fresh, gerr := s.store.Get(r.Context(), id); gerr == nil && parked(fresh.Status) {
		notice := fmt.Sprintf("📨 New message from %s. Run `warden msg inbox` to read.", from)
		if err := s.life.Input(r.Context(), fresh.TmuxSession, notice); err == nil {
			woke = true
		}
	}
	s.notify() // release any blocking waiters
	writeJSON(w, http.StatusCreated, sendMessageResponse{Message: m, Woke: woke})
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	unreadOnly := r.URL.Query().Get("unread") == "true"
	msgs, err := s.mbox.Messages(id)
	if errors.Is(err, mailbox.ErrBadRecipient) {
		writeErr(w, http.StatusBadRequest, "invalid recipient")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []mailbox.Message{}
	ids := []string{}
	for _, m := range msgs {
		if unreadOnly && m.Read {
			continue
		}
		out = append(out, m)
		if !m.Read {
			ids = append(ids, m.ID)
		}
	}
	// Read-then-mark is intentionally two lock acquisitions, not one: a racing
	// `msg wait` could consume a message in between, but that only ever
	// duplicates a display (the message is never lost), so a single combined
	// lock isn't worth the API. Don't "fix" this into one call.
	if len(ids) > 0 {
		if err := s.mbox.MarkRead(id, ids); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, inboxResponse{Messages: out})
}
