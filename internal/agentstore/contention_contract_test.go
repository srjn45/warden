package agentstore

// Acceptance gates for the lock-contention contract
// (docs/specs/2026-10-09-agent-store-lock-contention-contract.md §3-§8). Each is
// skipped until its owning task lands; the task removes the Skip and flips the
// matching TestBaseline* assertion in contention_baseline_test.go. The bodies
// are real so a gate cannot be un-skipped without delivering the behavior.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// I-1: a snapshot read never holds the write path.
func TestContractContentionScanDoesNotBlockWriters(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()
	g := slowRead(t)
	go func() { _, _ = s.List(ctx) }()
	g.waitEntered(t)
	blocked, wait := stalled(stallProbe, func() error { return s.UpdateStatus(ctx, "a-0", store.StatusIdle) })
	require.False(t, blocked, "writer must complete while a scan is in flight")
	_, err := wait()
	require.NoError(t, err)
}

// I-2: readers never wait for an in-flight write.
func TestContractContentionWriteDoesNotBlockReaders(t *testing.T) {
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
	s := seededStore(t, 5)
	ctx := context.Background()

	// 1. Off request path: an in-flight audit does NOT block foreground reads or writes.
	g := newGate()
	restore := SetAuditSeam(func(phase string) { g.hit() })
	t.Cleanup(func() { g.release(); restore() })

	go func() {
		_, _ = s.RunAudit(context.Background(), true)
	}()
	g.waitEntered(t)

	// Foreground read completes while audit is in flight.
	blocked, waitList := stalled(stallProbe, func() error {
		_, err := s.List(ctx)
		return err
	})
	require.False(t, blocked, "List must not block while audit is in flight")
	_, err := waitList()
	require.NoError(t, err)

	// Foreground write completes while audit is in flight.
	blocked, waitWrite := stalled(stallProbe, func() error {
		return s.UpdateStatus(ctx, "a-0", store.StatusIdle)
	})
	require.False(t, blocked, "UpdateStatus must not block while audit is in flight")
	_, err = waitWrite()
	require.NoError(t, err)

	g.release()

	// Store remains in StateOK when audit is clean.
	require.Equal(t, StateOK, s.State())

	// 2. Corrupt a record on disk in the active agents collection.
	entries, err := os.ReadDir(filepath.Join(s.DataDir(), "agents-db", "agents"))
	require.NoError(t, err)
	var segPath string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ndjson") || strings.HasSuffix(e.Name(), ".jsonl") || strings.Contains(e.Name(), "segment") {
			segPath = filepath.Join(s.DataDir(), "agents-db", "agents", e.Name())
			break
		}
	}
	require.NotEmpty(t, segPath, "must find a segment file")
	b, err := os.ReadFile(segPath)
	require.NoError(t, err)
	// Replace "a-0" in the record body with "corrupt-id" to produce a body ID mismatch.
	corrupted := strings.Replace(string(b), `"id":"a-0"`, `"id":"corrupt-id"`, 1)
	require.NotEqual(t, string(b), corrupted, "must replace id in segment")
	require.NoError(t, os.WriteFile(segPath, []byte(corrupted), 0o644))

	// Before the audit runs, the store is still StateOK (not degraded).
	require.Equal(t, StateOK, s.State())

	// A request-path notice can raise suspect, but cannot latch degraded.
	s.TransitionSuspect()
	require.Equal(t, StateSuspect, s.State())

	// 3. Auditor is the sole authority to detect corruption and atomically transition to degraded.
	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultFindings, rep.LastResult)
	require.NotEmpty(t, rep.Findings)

	// Store has transitioned to typed degraded state.
	require.Equal(t, StateDegraded, s.State())
	require.NotNil(t, s.DegradedError())

	// Subsequent foreground mutations are refused with typed UnhealthyError.
	err = s.UpdateStatus(ctx, "a-1", store.StatusIdle)
	require.Error(t, err)
	requireUnhealthy(t, err, store.DegradeIntegrity)
}
