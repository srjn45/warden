package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPendingSchema3TargetAuthorized rejects every partial or replayable
// variant of the readiness exception. The exception is deliberately derived
// entirely from durable transaction state and the currently executing binary;
// no environment switch can authorize an interrupted migration.
func TestPendingSchema3TargetAuthorized(t *testing.T) {
	makeFixture := func(t *testing.T) (string, *Ledger, string) {
		t.Helper()
		root, err := filepath.EvalSymlinks(t.TempDir())
		require.NoError(t, err)
		id := "txn123"
		exe, err := os.Executable()
		require.NoError(t, err)
		exe, err = filepath.EvalSymlinks(exe)
		require.NoError(t, err)
		body, err := os.ReadFile(exe)
		require.NoError(t, err)
		target := digest(body)
		manifest := struct {
			Format      int    `json:"format"`
			Transaction string `json:"transaction"`
			Migration   string `json:"migration"`
			Policy      string `json:"policy"`
			From        int    `json:"source_format"`
			To          int    `json:"target_format"`
			Capability  int    `json:"capability_version"`
			DataDir     string `json:"data_dir"`
			Version     string `json:"target_version"`
			Target      string `json:"target_binary_sha256"`
		}{1, id, "0003-ownership-normalization", "ownership-v1", 2, 3, 1, root, "target-test", target}
		raw, err := json.Marshal(manifest)
		require.NoError(t, err)
		backup := filepath.Join(root, "backups", "pre-schema3-"+id)
		require.NoError(t, os.MkdirAll(backup, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(backup, "MANIFEST.json"), raw, 0o600))
		manifestHash := digest(raw)
		type row struct {
			Seq         int    `json:"seq"`
			Previous    string `json:"previous"`
			Manifest    string `json:"manifest_hash"`
			Transaction string `json:"transaction"`
			Step        string `json:"step"`
			Complete    bool   `json:"complete"`
			Source      string `json:"source"`
			Staging     string `json:"staging"`
			Parked      string `json:"parked"`
			Target      string `json:"target_binary_sha256"`
			Input       string `json:"input"`
			Output      string `json:"output"`
		}
		r := row{Seq: 1, Manifest: manifestHash, Transaction: id, Step: "ledger_committed", Complete: true,
			Source: filepath.Join(root, WardenDBDir), Staging: filepath.Join(root, ".schema3-staging-"+id), Parked: filepath.Join(root, ".schema2-parked-"+id), Target: target}
		journalRaw, err := json.Marshal(r)
		require.NoError(t, err)
		journal := filepath.Join(root, ".schema3-transaction-"+id+".jsonl")
		require.NoError(t, os.WriteFile(journal, append(journalRaw, '\n'), 0o600))
		ledger := &Ledger{SchemaVersion: 3, BinaryVersion: manifest.Version, InProgress: &InProgress{Migration: manifest.Migration, TransactionID: id, ManifestSHA256: manifestHash, Journal: journal, Snapshot: backup, SourceFormat: 2, TargetFormat: 3, TargetBinarySHA256: target, Step: "ledger_committed"}}
		require.NoError(t, Save(root, ledger))
		return root, ledger, journal
	}

	t.Run("exact sealed transaction", func(t *testing.T) {
		root, ledger, _ := makeFixture(t)
		require.NoError(t, PendingSchema3TargetAuthorized(root, ledger))
	})
	for name, mutate := range map[string]func(t *testing.T, root string, l *Ledger, journal string){
		"unsafe transaction id": func(t *testing.T, _ string, l *Ledger, _ string) { l.InProgress.TransactionID = "../txn" },
		"wrong manifest seal":   func(t *testing.T, _ string, l *Ledger, _ string) { l.InProgress.ManifestSHA256 = "sha256:bad" },
		"wrong executable hash": func(t *testing.T, _ string, l *Ledger, _ string) { l.InProgress.TargetBinarySHA256 = "sha256:bad" },
		"wrong target version":  func(t *testing.T, _ string, l *Ledger, _ string) { l.BinaryVersion = "wrong-target" },
		"wrong snapshot":        func(t *testing.T, _ string, l *Ledger, _ string) { l.InProgress.Snapshot = "backups/other" },
		"replayed journal sequence": func(t *testing.T, _ string, _ *Ledger, journal string) {
			b, err := os.ReadFile(journal)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(journal, append(b, b...), 0o600))
		},
		"unterminated journal": func(t *testing.T, _ string, _ *Ledger, journal string) {
			b, err := os.ReadFile(journal)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(journal, b[:len(b)-1], 0o600))
		},
		"wrong final stage": func(t *testing.T, _ string, _ *Ledger, journal string) {
			b, err := os.ReadFile(journal)
			require.NoError(t, err)
			var r map[string]any
			require.NoError(t, json.Unmarshal(b, &r))
			r["step"] = "readiness_started"
			raw, err := json.Marshal(r)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(journal, append(raw, '\n'), 0o600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, ledger, journal := makeFixture(t)
			mutate(t, root, ledger, journal)
			require.Error(t, PendingSchema3TargetAuthorized(root, ledger))
		})
	}
}

func TestPendingSchema3TargetAuthorizedDigestFormat(t *testing.T) {
	s := sha256.Sum256([]byte("x"))
	require.Equal(t, "sha256:"+hex.EncodeToString(s[:]), digest([]byte("x")))
}
