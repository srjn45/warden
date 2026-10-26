package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

func TestFinalizedSchema3ReadinessRequiresExactCompletedJournal(t *testing.T) {
	dir := t.TempDir()
	pending := schema.InProgress{
		TransactionID:      "tx",
		ManifestSHA256:     "sha256:manifest",
		TargetBinarySHA256: "sha256:target",
		Journal:            filepath.Join(dir, ".schema3-transaction-tx.jsonl"),
	}
	ledger := &schema.Ledger{SchemaVersion: schema.SchemaVersion, BinaryVersion: "13.0.0"}
	valid := `{"transaction":"tx","manifest_hash":"sha256:manifest","target_binary_sha256":"sha256:target","step":"readiness_verified","complete":true}` + "\n"

	write := func(body string) {
		require.NoError(t, os.WriteFile(pending.Journal, []byte(body), 0o600))
	}
	write(valid)
	require.NoError(t, finalizedSchema3Readiness(ledger, pending, "13.0.0"))

	for name, body := range map[string]string{
		"unterminated":      valid[:len(valid)-1],
		"foreign txn":       `{"transaction":"other","manifest_hash":"sha256:manifest","target_binary_sha256":"sha256:target","step":"readiness_verified","complete":true}` + "\n",
		"unfinished record": `{"transaction":"tx","manifest_hash":"sha256:manifest","target_binary_sha256":"sha256:target","step":"readiness_verified","complete":false}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			write(body)
			require.Error(t, finalizedSchema3Readiness(ledger, pending, "13.0.0"))
		})
	}

	write(valid)
	ledger.InProgress = &pending
	require.Error(t, finalizedSchema3Readiness(ledger, pending, "13.0.0"))
}
