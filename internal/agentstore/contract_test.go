package agentstore

// Executable storage contract for issue #795; see
// docs/specs/2026-10-06-agent-store-integrity-contract.md. Tests named
// TestContract* that are skipped describe behavior later tasks must deliver;
// removing the Skip is the acceptance gate for that task.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

const holderEnv = "WARDEN_AGENTSTORE_HOLDER_DIR"

// TestContractHolderProcess is a helper, not a test: when re-executed with
// holderEnv it opens the store, prints READY and blocks until stdin closes.
func TestContractHolderProcess(t *testing.T) {
	dir := os.Getenv(holderEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	s, err := New(dir)
	if err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		return
	}
	defer s.Close()
	os.Stdout.WriteString("READY\n")
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
}

// TestContractScrivaDependencyPin fails when scriva is bumped so the owner of
// the bump must revisit the contract doc. v1.2.1 exports no lock, Verify or
// Repair surface; warden must not emulate them (contract §2).
func TestContractScrivaDependencyPin(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	m := regexp.MustCompile(`github.com/srjn45/scriva (v\S+)`).FindSubmatch(gomod)
	require.NotNil(t, m)
	require.Equal(t, "v1.4.0", string(m[1]),
		"scriva bumped: re-audit docs/specs/2026-10-06-agent-store-integrity-contract.md §2 and update this pin")

	_, err = engine.VerifyDir(context.Background(), t.TempDir(), engine.VerifyOptions{})
	require.NoError(t, err)
}

// TestContractLegacyLockDoesNotCoverAgentStore pins that the legacy
// .sessions-store.lock held by store.FileStore does not protect agents-db, so
// the new lock must be its own file (contract §3).
func TestContractLegacyLockDoesNotCoverAgentStore(t *testing.T) {
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	defer legacy.Close(context.Background())
	// A live legacy owner blocks the import read, but the agent lock itself is
	// a separate file.
	_, err = New(dir)
	require.ErrorIs(t, err, store.ErrStoreOwned)
	require.FileExists(t, filepath.Join(dir, ".agents-store.lock"))
	require.FileExists(t, filepath.Join(dir, ".sessions-store.lock"))
}

// TestContractExclusiveOwnership: same-process and cross-process second
// openers get an ownership error without touching data; Close or process exit
// releases the lock.
func TestContractExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir)
	require.NoError(t, err)

	_, err = New(dir)
	require.ErrorIs(t, err, store.ErrStoreOwned)

	cmd := exec.Command(os.Args[0], "-test.run=TestContractHolderProcess")
	cmd.Env = append(os.Environ(), holderEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, string(out), "ERR", "cross-process opener must be rejected")

	require.NoError(t, first.Close())
	again, err := New(dir)
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

// TestContractListCompleteOrError: a nil-error engine scan that returns fewer
// rows than the primary index holds must surface as an error, never a short
// list (contract §4).
func TestContractListCompleteOrError(t *testing.T) {
	dir := seeded(t)
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		ids := byOffset(m)
		e := m[ids[len(ids)-1]]
		e.Offset++
		m[ids[len(ids)-1]] = e
	})
	s := reopen(t, dir)
	got, err := s.List(context.Background())
	require.Nil(t, got, "a corrupt index must never produce a partial fleet")
	require.ErrorIs(t, err, ErrUnhealthy)
	_, ok := store.IsDegraded(err)
	require.True(t, ok, "the existing degraded-store boundary must remain usable")
}

// TestContractRepairConflicts: repair preserves duplicate logical IDs as
// conflicts and never selects one silently (contract §5).
func TestContractRepairConflicts(t *testing.T) {
	t.Skip("pending: repair task (#795)")
}
