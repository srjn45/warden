package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// selection is the cross-pane state written by the list pane and read by the
// detail pane. It lives in the cockpit's per-pid state dir, never the daemon.
type selection struct {
	ID string `json:"id"`
	TS int64  `json:"ts"`
}

func selectionPath(stateDir string) string {
	return filepath.Join(stateDir, "selection.json")
}

// writeSelection atomically records the selected agent id. An empty stateDir is
// a no-op (classic mode has no cockpit state dir).
func writeSelection(stateDir, id string, ts int64) error {
	if stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(selection{ID: id, TS: ts})
	if err != nil {
		return err
	}
	tmp := selectionPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, selectionPath(stateDir))
}

// readSelection returns the selected id, or "" if unset, missing, or corrupt.
func readSelection(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	b, err := os.ReadFile(selectionPath(stateDir))
	if err != nil {
		return ""
	}
	var s selection
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.ID
}
