package planbackup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/srjn45/warden/internal/planstore"
)

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("planbackup: hash marshal: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}

type eventHashRow struct {
	ID          string                  `json:"id"`
	DedupKey    string                  `json:"dedup_key,omitempty"`
	PlanID      string                  `json:"plan_id"`
	ExecutionID string                  `json:"execution_id"`
	Kind        planstore.EventKind     `json:"kind"`
	Seq         int64                   `json:"seq"`
	OccurredAt  string                  `json:"occurred_at"`
	Payload     *planstore.EventPayload `json:"payload,omitempty"`
}

type noteHashRow struct {
	ID          string `json:"id"`
	PlanID      string `json:"plan_id"`
	ExecutionID string `json:"execution_id"`
	AgentID     string `json:"agent_id"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
}

func hashEvents(events []*planstore.PlanExecutionEvent) string {
	rows := make([]eventHashRow, 0, len(events))
	for _, ev := range events {
		if ev == nil {
			continue
		}
		rows = append(rows, eventHashRow{
			ID:          ev.ID,
			DedupKey:    ev.DedupKey,
			PlanID:      ev.PlanID,
			ExecutionID: ev.ExecutionID,
			Kind:        ev.Kind,
			Seq:         ev.Seq,
			OccurredAt:  ev.OccurredAt.UTC().Format(timeRFC3339Nano),
			Payload:     ev.Payload,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Seq != rows[j].Seq {
			return rows[i].Seq < rows[j].Seq
		}
		if rows[i].ID != rows[j].ID {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].DedupKey < rows[j].DedupKey
	})
	return hashJSON(rows)
}

func hashNotes(notes []*planstore.ExecutionNote) string {
	rows := make([]noteHashRow, 0, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		rows = append(rows, noteHashRow{
			ID:          n.ID,
			PlanID:      n.PlanID,
			ExecutionID: n.ExecutionID,
			AgentID:     n.AgentID,
			Content:     n.Content,
			CreatedAt:   n.CreatedAt.UTC().Format(timeRFC3339Nano),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt < rows[j].CreatedAt
		}
		return rows[i].ID < rows[j].ID
	})
	return hashJSON(rows)
}

type entryHashPayload struct {
	PlanID          string `json:"plan_id"`
	Revision        int64  `json:"revision"`
	PlanContentHash string `json:"plan_content_hash"`
	EventsHash      string `json:"events_hash"`
	NotesHash       string `json:"notes_hash"`
}

func hashEntry(planID string, revision int64, planHash, eventsHash, notesHash string) string {
	return hashJSON(entryHashPayload{
		PlanID:          planID,
		Revision:        revision,
		PlanContentHash: planHash,
		EventsHash:      eventsHash,
		NotesHash:       notesHash,
	})
}

type bundleHashEntry struct {
	PlanID    string `json:"plan_id"`
	EntryHash string `json:"entry_hash"`
}

// Seal fills per-entry integrity hashes and BundleHash. Call after Entries are
// populated (and sanitized). Idempotent for the same Entries content.
func Seal(b *Bundle) {
	if b == nil {
		return
	}
	rows := make([]bundleHashEntry, 0, len(b.Entries))
	for i := range b.Entries {
		e := &b.Entries[i]
		if e.Plan == nil {
			continue
		}
		planstore.RefreshContentHash(e.Plan)
		e.PlanContentHash = e.Plan.ContentHash
		e.EventsHash = hashEvents(e.Events)
		e.NotesHash = hashNotes(e.Notes)
		e.EntryHash = hashEntry(e.Plan.ID, e.Plan.Revision, e.PlanContentHash, e.EventsHash, e.NotesHash)
		rows = append(rows, bundleHashEntry{PlanID: e.Plan.ID, EntryHash: e.EntryHash})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PlanID < rows[j].PlanID })
	b.BundleHash = hashJSON(rows)
}

const timeRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
