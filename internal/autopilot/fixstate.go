package autopilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// FixState is the daemon-written fix-loop record for one task (run-to-final-pr
// spec §B.6), stored at autopilot.<run>.fix.<task>. It makes every dispatch
// restart-safe: LastDispatchedSHA is written BEFORE the send/spawn, so a daemon
// restart never repeats a dispatch for the same red head SHA.
//
// "fixing" is deliberately a property of this record (Fixing), not a canonical
// ledger task state: the canonical states are the TUI segmentation order.
type FixState struct {
	Task              string `json:"task"`
	PR                int    `json:"pr,omitempty"`
	Branch            string `json:"branch,omitempty"`
	HeadSHA           string `json:"head_sha,omitempty"`
	Kind              string `json:"kind,omitempty"` // red | conflict
	RerunDoneForSHA   string `json:"rerun_done_for_sha,omitempty"`
	Reruns            int    `json:"reruns,omitempty"` // total CI re-runs for this task
	LastDispatchedSHA string `json:"last_dispatched_sha,omitempty"`
	FixAttempts       int    `json:"fix_attempts,omitempty"` // total fix dispatches for this task
	RedStreak         int    `json:"red_streak,omitempty"`   // consecutive red head SHAs
	ResolverCalledSHA string `json:"resolver_called_sha,omitempty"`
	CappedAuditedSHA  string `json:"capped_audited_sha,omitempty"`
	BusySince         string `json:"busy_since,omitempty"` // RFC3339; owner alive but working
	Evidence          string `json:"evidence,omitempty"`   // trimmed, for HeadSHA
	FixerSession      string `json:"fixer_session,omitempty"`
	Fixing            bool   `json:"fixing,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// FixStateKey is the ctx key for one task's fix state.
func (l *Ledger) FixStateKey(taskID string) string {
	return l.key("fix." + taskID)
}

// FixState reads a task's fix state; a zero value (Task set) when absent.
func (l *Ledger) FixState(taskID string) (FixState, error) {
	if strings.TrimSpace(taskID) == "" {
		return FixState{}, fmt.Errorf("%w: empty id", ErrInvalidLedgerTask)
	}
	fs := FixState{Task: taskID}
	if err := l.readJSON(l.FixStateKey(taskID), &fs); err != nil {
		return FixState{}, err
	}
	fs.Task = taskID
	return fs, nil
}

// WriteFixState persists a task's fix state.
func (l *Ledger) WriteFixState(fs FixState, by string) error {
	if strings.TrimSpace(fs.Task) == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidLedgerTask)
	}
	b, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	return l.store.Set(l.FixStateKey(fs.Task), string(b), writerOrDefault(by))
}

// ClearFixState resets the red streak and outstanding-fix flag when a task's PR
// lands, keeping the lifetime counters (reruns, attempts) for the audit trail.
func (l *Ledger) ClearFixState(taskID, by string) error {
	fs, err := l.FixState(taskID)
	if err != nil {
		return err
	}
	if fs.HeadSHA == "" && fs.RedStreak == 0 && !fs.Fixing {
		return nil
	}
	fs.RedStreak, fs.Fixing, fs.Evidence, fs.BusySince = 0, false, "", ""
	return l.WriteFixState(fs, by)
}

var errNoLedger = errors.New("autopilot fix loop: no ledger")
