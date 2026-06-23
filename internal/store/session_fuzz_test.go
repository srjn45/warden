package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FuzzReadSession exercises the on-disk session decoder against arbitrary file
// contents. readSession loads JSON the daemon wrote, but a record can be
// corrupted (partial write survived from a pre-atomic-write version, manual
// edit, disk fault), so the contract under fuzzing is: never panic, and any
// session it successfully decodes must re-marshal cleanly (no value that round-
// trips into a form json/Marshal then rejects).
func FuzzReadSession(f *testing.F) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	good, _ := json.Marshal(&Session{
		ID:        "abc123",
		Type:      TypeDevelopment,
		Status:    StatusWorking,
		Repo:      "/repo",
		CreatedAt: now,
		UpdatedAt: now,
		Events:    []Event{{TS: now, Type: "spawn", Detail: "started"}},
	})
	seeds := [][]byte{
		good,
		[]byte("{}"),
		[]byte(`{"id":"x","status":"bogus","type":"???"}`),
		[]byte(`{"id":"x","exit_code":7,"events":null}`),
		[]byte("not json at all"),
		[]byte(""),
		[]byte("[1,2,3]"),
		[]byte(`{"created_at":"not-a-time"}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	dir := f.TempDir()
	path := filepath.Join(dir, "fuzz.json")

	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write temp session: %v", err)
		}
		s, err := readSession(path)
		if err != nil {
			if s != nil {
				t.Fatalf("readSession returned a session alongside an error: %+v", s)
			}
			return
		}
		if s == nil {
			t.Fatal("readSession returned nil session with nil error")
		}
		// A decoded record must survive a re-marshal: the daemon rewrites
		// sessions in place, so an undecodable-on-write value would corrupt
		// the store on the next mutation.
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("decoded session does not re-marshal: %v\ninput: %q", err, data)
		}
	})
}
