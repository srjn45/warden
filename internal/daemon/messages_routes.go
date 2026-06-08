package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/mailbox"
	"github.com/srajanpathak/agentctl/internal/store"
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
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
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
	from := req.From
	if from == "" {
		from = "human"
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
	woke := false
	if parked(sess.Status) {
		notice := fmt.Sprintf("📨 New message from %s. Run `agentctl msg inbox` to read.", from)
		if err := s.life.Input(r.Context(), sess.TmuxSession, notice); err == nil {
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
