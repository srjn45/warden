package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/store"
)

// outputStreamInterval is how often the handler re-captures the agent's pane.
const outputStreamInterval = time.Second

// handleOutputStream streams a single agent's tmux pane as SSE. Each frame is a
// JSON-encoded OutputResponse (so embedded newlines / ANSI escapes survive SSE
// line framing). It sends an immediate first frame, then a new one whenever the
// pane changes, on a ~1s tick.
func (s *Server) handleOutputStream(w http.ResponseWriter, r *http.Request) {
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var last string
	send := func() bool {
		out, err := s.life.OutputANSI(r.Context(), sess.TmuxSession, 200)
		if err != nil {
			return true // transient (e.g. pane gone mid-poll); try again next tick
		}
		if out == last {
			return true
		}
		last = out
		payload, err := json.Marshal(OutputResponse{Output: out})
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(outputStreamInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
