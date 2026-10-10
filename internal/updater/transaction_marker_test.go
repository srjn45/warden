package updater

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

// The updater journals a snapshot marker in schema.json before swapping the
// binary. The new daemon runs the boot guard against that same ledger on
// restart, so the marker must be gone (and must never read as an interrupted
// migration) by then.
func TestTxnSnapshotMarkerDoesNotBlockNewDaemonBoot(t *testing.T) {
	dataDir := t.TempDir()
	require.NoError(t, schema.Save(dataDir, &schema.Ledger{SchemaVersion: schema.SchemaVersion, BinaryVersion: "1.0.0"}))

	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 0 {
				return ok("1.0.0")
			}
			return ok("2.0.0")
		}
	})
	e.txn.opts.DataDir = dataDir
	e.txn.opts.Migrate = func() error { return nil }
	e.txn.pre.DaemonRunning = true

	var atRestart *schema.Ledger
	var guardErr error
	e.svc.restartErr = func(int) error {
		l, err := schema.Load(dataDir)
		require.NoError(t, err)
		atRestart = l
		guardErr = schema.Check(dataDir, l)
		return guardErr
	}

	rb, err := e.run()
	require.NoError(t, err)
	require.False(t, rb)
	require.NoError(t, guardErr, "new daemon's boot guard must accept the ledger the updater leaves behind")
	require.NotNil(t, atRestart)
	require.Nil(t, atRestart.InProgress, "snapshot marker must be cleared before the service restarts")
	require.Equal(t, "new", e.binary())
	_, statErr := os.Stat(filepath.Join(dataDir, "schema.json"))
	require.NoError(t, statErr)
}
