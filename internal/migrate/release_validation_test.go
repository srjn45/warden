package migrate

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// TestSchema2ReleaseAcceptance exercises the operator-visible happy path at a
// non-trivial size.  The focused phase/crash tests remain in migrate_test.go;
// this test proves that the real registered cutover joins those guarantees:
// an old install is promoted into a restartable schema-2 state and is rejected
// by a schema-1 binary afterwards.
func TestSchema2ReleaseAcceptance(t *testing.T) {
	dir := stagingLegacyDir(t)
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "v-schema1"}))
	seedLargeAgentsStore(t, dir, 512)

	runner := NewRunner(DefaultRegistry)
	env := Env{DataDir: dir, BinaryVersion: "v-release-test", Context: context.Background()}
	require.NoError(t, runner.Apply(env, schema.Schema2))

	ledger, err := schema.Load(dir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, ledger.SchemaVersion)
	require.Nil(t, ledger.InProgress)
	require.Len(t, ledger.History, 1)
	require.Equal(t, Migration0002ID, ledger.History[0].Migration)

	// This is the restart boundary for the schema-2-capable daemon: a completed
	// ledger starts without another migration. SchemaVersion deliberately remains
	// 1 until the schema-2 runtime is enabled in its own release.
	require.Equal(t, schema.VerdictStart, schema.Decide(ledger.SchemaVersion, schema.Schema2, 1, false))

	// A schema-1 binary must refuse the promoted directory rather than opening
	// its still-present legacy stores alongside warden-db.
	require.Equal(t, schema.VerdictDataNewer, schema.Decide(ledger.SchemaVersion, 1, 0, false))

	// The promoted target remains independently verifiable after restart.
	report, err := VerifySchema2Equivalence(context.Background(), dir, wardendb.Dir(dir))
	require.NoError(t, err)
	require.True(t, report.Passed)
	require.Equal(t, 514, report.TargetCounts["agents"])
}

func seedLargeAgentsStore(t *testing.T, dataDir string, n int) {
	t.Helper()
	root := filepath.Join(dataDir, "agents-db")
	col, err := engine.OpenCollection("agents", root, engine.CollectionConfig{})
	require.NoError(t, err)
	defer col.Close()
	for i := 0; i < n; i++ {
		_, _, err := col.Insert(map[string]any{"id": fmt.Sprintf("agent-%03d", i), "project_id": "/repo", "v": i})
		require.NoError(t, err)
	}
}
