package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

func (s *Server) registerMessageRoutes(r chi.Router) {
	r.Post("/sessions/{id}/messages", s.handleSendMessage)
	r.Get("/sessions/{id}/messages", s.handleInbox)
	r.Get("/sessions/{id}/messages/wait", s.handleWaitMessage)
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
	if len(ids) > 0 {
		if err := s.mbox.MarkRead(id, ids); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, inboxResponse{Messages: out})
}
