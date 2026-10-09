package agentstore

// Acceptance gates for the lock-contention contract
// (docs/specs/2026-10-09-agent-store-lock-contention-contract.md §3-§8). Each is
// skipped until its owning task lands; the task removes the Skip and flips the
// matching TestBaseline* assertion in contention_baseline_test.go. The bodies
// are real so a gate cannot be un-skipped without delivering the behavior.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// I-1: a snapshot read never holds the write path.
func TestContractContentionScanDoesNotBlockWriters(t *testing.T) {
	t.Skip("gate: snapshot reads (contract I-1) — flips TestBaselineSlowScanBlocksWriters")
	s := seededStore(t, 3)
	ctx := context.Background()
	g := slowScan(t)
	go func() { _, _ = s.List(ctx) }()
	g.waitEntered(t)
	blocked, wait := stalled(stallProbe, func() error { return s.UpdateStatus(ctx, "a-0", store.StatusIdle) })
	require.False(t, blocked, "writer must complete while a scan is in flight")
	_, err := wait()
	require.NoError(t, err)
}

// I-2: readers never wait for an in-flight write.
func TestContractContentionWriteDoesNotBlockReaders(t *testing.T) {
	t.Skip("gate: snapshot reads (contract I-2) — flips TestBaselineSlowWriteBlocksReaders")
	s := seededStore(t, 3)
	ctx := context.Background()
	g := slowWrite(t, "Update")
	go func() { _ = s.UpdateStatus(ctx, "a-0", store.StatusIdle) }()
	g.waitEntered(t)
	blocked, wait := stalled(stallProbe, func() error { _, err := s.List(ctx); return err })
	require.False(t, blocked)
	_, err := wait()
	require.NoError(t, err)
}

// I-3: point reads are O(1) — no collection scan.
func TestContractContentionGetIsPointRead(t *testing.T) {
	t.Skip("gate: point reads (contract §5) — flips TestBaselineScanCountPerOperation")
	s := seededStore(t, 5)
	n := countScans(t)
	_, err := s.Get(context.Background(), "a-1")
	require.NoError(t, err)
	require.Zero(t, n.Load(), "Get must not scan the collection")
}

// I-5: a caller whose ctx expires while queued returns ctx.Err() at the deadline.
func TestContractContentionContextBoundsQueuedCalls(t *testing.T) {
	t.Skip("gate: cancellation (contract §7) — flips TestBaselineContextIgnoredWhileQueued")
	s := seededStore(t, 2)
	g := slowWrite(t, "Update")
	go func() { _ = s.UpdateStatus(context.Background(), "a-0", store.StatusIdle) }()
	g.waitEntered(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.List(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 200*time.Millisecond)
}

// I-6: while degraded the store serves a labelled stale snapshot, never a
// partial one and never silence.
func TestContractContentionDegradedServesLabelledStale(t *testing.T) {
	t.Skip("gate: stale/degraded semantics (contract §4) — no API exists yet; task defines SnapshotMeta")
}

// §6: periodic integrity audit runs off the request path and is the only
// authority that can flip the store to degraded for non-read-failure causes.
func TestContractContentionAuditIsOffRequestPath(t *testing.T) {
	t.Skip("gate: integrity-audit authority (contract §6) — no auditor exists yet")
}
