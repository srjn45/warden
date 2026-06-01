package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

// handleEventsStream streams the full session list as SSE. It sends an initial
// snapshot, then a new one whenever the hub fires (deduped), plus a heartbeat.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.hub.subscribe()
	defer unsub()

	var last []byte
	send := func() bool {
		sessions, err := s.store.List(r.Context())
		if err != nil {
			return true // transient; try again on next signal
		}
		if sessions == nil {
			sessions = []*store.Session{}
		}
		payload, err := json.Marshal(sessionsResponse{Sessions: sessions})
		if err != nil {
			return true
		}
		if bytes.Equal(payload, last) {
			return true // nothing changed since last send
		}
		last = payload
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
