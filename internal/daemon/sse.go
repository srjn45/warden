package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/store"
)

const (
	sseEventTree  = "tree"
	sseEventError = "error"
)

// sseErrorPayload is the wire contract for the named `error` event. The
// unnamed/default event remains reserved for complete session snapshots.
type sseErrorPayload struct {
	Error    string `json:"error"`
	Degraded bool   `json:"degraded"`
}

// handleEventsStream streams the full session list as SSE. It sends an initial
// snapshot, then a new one whenever the hub fires (deduped), plus a heartbeat.
// The unnamed/default event carries only session snapshots; `tree` and `error`
// are named events with their own payload schemas.
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

	all := r.URL.Query().Get("all") == "true"

	// Two independently-deduplicated frames ride this one stream (spec §10): the
	// sessions snapshot as the UNNAMED default event (unchanged, back-compat) and
	// the project tree as a NAMED `tree` event. Each keeps its own last-sent bytes
	// so a pure status change re-sends only sessions and a structural change
	// re-sends only the tree — keeping the phone payload small over a relay.
	var lastSess, lastTree []byte
	send := func() bool {
		sessions, err := s.store.List(r.Context())
		if err != nil {
			// Complete-or-error: a degraded active scan must NOT publish a partial
			// snapshot. Keep the last complete payloads (do not touch lastSess/lastTree)
			// and wait for a later clean scan — the SSE consumer keeps showing
			// last-known-good. On a degraded INITIAL scan both buffers are still empty,
			// so nothing is emitted until the first complete read, which is exactly the
			// desired behavior.
			if d, ok := store.IsDegraded(err); ok {
				logStoreDegraded(d)
				// An initial degraded stream must be explicit: silence looks like a
				// healthy empty fleet to EventSource consumers. Existing snapshots
				// remain intact; clients can retain them while showing the error.
				payload, _ := json.Marshal(sseErrorPayload{Error: d.Error(), Degraded: true})
				if _, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseEventError, payload); werr != nil {
					return false
				}
				flusher.Flush()
			}
			return true // keep the stream open; retry on the next signal
		}
		visible := make([]*store.Session, 0, len(sessions))
		for _, sess := range sessions {
			if !all && sess.HasTag("system:true") {
				continue
			}
			visible = append(visible, sess.ToSession())
		}
		// Gather the tree inputs once; the autopilot status it computed feeds both
		// frames (spec §10 — one Status() call, no double computation).
		in := s.treeInputsFor(visible)

		// Sessions frame (unnamed). Autopilot rides the fleet stream so cockpit run
		// trees update on the same notification as their brain/worker/guardian
		// sessions. The field is additive: older clients continue decoding only
		// `sessions`.
		frame := struct {
			Sessions  []*store.Session `json:"sessions"`
			Autopilot any              `json:"autopilot,omitempty"`
		}{Sessions: visible}
		if s.autopilot != nil {
			frame.Autopilot = in.Autopilot
		}
		if payload, merr := json.Marshal(frame); merr == nil && !bytes.Equal(payload, lastSess) {
			lastSess = payload
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return false
			}
			flusher.Flush()
		}

		// Tree frame (named `tree`, same shape as GET /api/v1/tree). ?all applies to
		// both frames — `sessions` is already filtered above and feeds the tree.
		t := treeService.Build(in, "")
		if tpayload, merr := json.Marshal(t); merr == nil && !bytes.Equal(tpayload, lastTree) {
			lastTree = tpayload
			if _, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sseEventTree, tpayload); werr != nil {
				return false
			}
			flusher.Flush()
		}
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
		case <-s.done: // server shutting down — close the stream so Shutdown can drain
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
