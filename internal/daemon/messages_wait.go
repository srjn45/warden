package daemon

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/warden/internal/mailbox"
)

const (
	defaultWaitSec = 300 // 5 min default long-poll window
	maxWaitSec     = 600 // hard cap
)

// waitResponse is the body for GET /sessions/{id}/messages/wait.
type waitResponse struct {
	Found   bool             `json:"found"`
	Message *mailbox.Message `json:"message,omitempty"`
}

// handleWaitMessage long-polls: it subscribes to the hub and returns as soon as
// an unread message for {id} (optionally filtered by ?from=) arrives, or returns
// found=false when ?timeout= (default 300s, capped 600s) elapses. This is what
// lets an agent await a reply in a single Bash call with no LLM-turn busy-poll.
func (s *Server) handleWaitMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	from := r.URL.Query().Get("from")
	timeoutSec := defaultWaitSec
	if q := r.URL.Query().Get("timeout"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			timeoutSec = n
		}
	}
	if timeoutSec > maxWaitSec {
		timeoutSec = maxWaitSec
	}

	ch, unsub := s.hub.subscribe()
	defer unsub()

	deadline := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer deadline.Stop()

	for {
		m, ok, err := s.mbox.TakeFirstUnread(id, from)
		if errors.Is(err, mailbox.ErrBadRecipient) {
			writeErr(w, http.StatusBadRequest, "invalid recipient")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, waitResponse{Found: true, Message: &m})
			return
		}
		select {
		case <-r.Context().Done():
			return // client hung up
		case <-s.done:
			writeJSON(w, http.StatusOK, waitResponse{Found: false})
			return
		case <-deadline.C:
			writeJSON(w, http.StatusOK, waitResponse{Found: false})
			return
		case <-ch:
			// something changed — loop and re-check the inbox
		}
	}
}
