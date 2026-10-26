package schema

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchema3GuardRecoveryUsesUpdaterAuthority(t *testing.T) {
	err := &GuardError{
		Verdict: VerdictInterrupted, DataDir: "/fixture", Binary: 3,
		Ledger: Ledger{SchemaVersion: 3, InProgress: &InProgress{
			Migration: "0003-ownership-normalization", TransactionID: "selected-update", TargetFormat: 3, Step: "ledger_committed",
		}},
	}
	message := err.Error()
	require.Contains(t, message, "warden update")
	require.Contains(t, message, "warden rollback --transaction selected-update --accept-data-loss")
	require.NotContains(t, message, "migrate --resume")
	require.NotContains(t, message, "migrate --restore")
	err.Verdict = VerdictDataNewer
	err.Binary = 2
	require.Contains(t, err.Error(), "exact compatible binary")
	require.Contains(t, err.Error(), "paired old binary")
	require.NotContains(t, err.Error(), "only knows the schema-1 stores")
}
