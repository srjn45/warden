package agentstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// Helper to find the active segment file in the store's agents collection.
func findSegmentFile(t testing.TB, s *Store, col string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.DataDir(), "agents-db", col))
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ndjson") || strings.HasSuffix(e.Name(), ".jsonl") || strings.Contains(e.Name(), "segment") {
			return filepath.Join(s.DataDir(), "agents-db", col, e.Name())
		}
	}
	t.Fatalf("no segment file found in %s", col)
	return ""
}

// 1. Detection of silent index omission (count != scan).
func TestAuditorDetectionSilentOmission(t *testing.T) {
	dir := seeded(t)
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		ids := byOffset(m)
		e := m[ids[2]]
		e.Offset++
		m[ids[2]] = e
	})
	s := reopen(t, dir)
	ctx := context.Background()

	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultFindings, rep.LastResult)

	var foundOmission bool
	for _, f := range rep.Findings {
		if strings.Contains(f.Detail, "silent omission") || strings.Contains(f.Detail, "offset") || f.Class == string(store.DegradeIntegrity) {
			foundOmission = true
			break
		}
	}
	require.True(t, foundOmission, "must detect index omission / corrupt offset in findings: %+v", rep.Findings)
	require.Equal(t, StateDegraded, s.State())
	require.NotNil(t, s.DegradedError())
}

// 2. Detection of identity mismatch (body ID does not match logical key).
func TestAuditorDetectionIdentityMismatch(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	segPath := findSegmentFile(t, s, "agents")
	b, err := os.ReadFile(segPath)
	require.NoError(t, err)

	corrupted := strings.Replace(string(b), `"id":"a-1"`, `"id":"wrong-id"`, 1)
	require.NotEqual(t, string(b), corrupted)
	require.NoError(t, os.WriteFile(segPath, []byte(corrupted), 0o644))

	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultFindings, rep.LastResult)

	var foundMismatch bool
	for _, f := range rep.Findings {
		if strings.Contains(f.Detail, "does not match") || strings.Contains(f.Detail, "mismatch") || f.Class == string(store.DegradeIntegrity) {
			foundMismatch = true
			break
		}
	}
	require.True(t, foundMismatch, "must detect identity mismatch: %+v", rep.Findings)
	require.Equal(t, StateDegraded, s.State())
}

// 3. Detection of duplicate identity.
func TestAuditorDetectionDuplicateIdentity(t *testing.T) {
	dir := seeded(t)
	rewriteIndex(t, dir, "agents", func(m map[string]idxEntry) {
		ids := byOffset(m)
		m[ids[0]] = m[ids[1]]
	})
	s := reopen(t, dir)
	ctx := context.Background()

	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultFindings, rep.LastResult)
	require.NotEmpty(t, rep.Findings)
	require.Equal(t, StateDegraded, s.State())
}

// 4. Detection of decode failure (undecodable JSON in active collection).
func TestAuditorDetectionDecodeFailure(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	segPath := findSegmentFile(t, s, "agents")
	b, err := os.ReadFile(segPath)
	require.NoError(t, err)

	// Corrupt JSON syntax in one record body.
	corrupted := strings.Replace(string(b), `"status":"working"`, `"status":{unclosed`, 1)
	require.NotEqual(t, string(b), corrupted)
	require.NoError(t, os.WriteFile(segPath, []byte(corrupted), 0o644))

	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultFindings, rep.LastResult)

	var foundDecode bool
	for _, f := range rep.Findings {
		if f.Class == string(store.DegradeDecode) || f.Class == string(store.DegradeIntegrity) {
			foundDecode = true
			break
		}
	}
	require.True(t, foundDecode, "must report decode or integrity failure on corrupted record JSON: %+v", rep.Findings)
	require.Equal(t, StateDegraded, s.State())
}

// 5. Single-flight: concurrent audit calls join into 1 execution pass.
func TestAuditorSingleFlight(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	var auditRuns atomic.Int64
	restore := SetAuditSeam(func(phase string) {
		if phase == "start_attempt" {
			auditRuns.Add(1)
			time.Sleep(50 * time.Millisecond)
		}
	})
	defer restore()

	const concurrency = 8
	var wg sync.WaitGroup
	reports := make([]*AuditReport, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rep, err := s.RunAudit(ctx, true)
			require.NoError(t, err)
			reports[idx] = rep
		}(i)
	}

	wg.Wait()

	require.Equal(t, int64(1), auditRuns.Load(), "concurrent audits must coalesce via single-flight")
	for i := 1; i < concurrency; i++ {
		require.Equal(t, reports[0].LastRunAt, reports[i].LastRunAt, "all callers should receive identical report")
	}
}

// 6. Rate-limited debounce: unforced audit calls within debounce window return cached report.
func TestAuditorRateLimitedDebounce(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	rep1, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultClean, rep1.LastResult)

	var attempted atomic.Bool
	restore := SetAuditSeam(func(phase string) {
		attempted.Store(true)
	})
	defer restore()

	// Call with force=false immediately: should return cached report without invoking verification.
	rep2, err := s.RunAudit(ctx, false)
	require.NoError(t, err)
	require.False(t, attempted.Load(), "debounced call must not invoke verification pass")
	require.Equal(t, rep1.LastRunAt, rep2.LastRunAt)

	// Call with force=true: should bypass debounce and run verification pass.
	rep3, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.True(t, attempted.Load(), "forced call must run verification pass")
	require.True(t, rep3.LastRunAt.After(rep1.LastRunAt) || rep3.LastRunAt.Equal(rep1.LastRunAt))
}

// 7. Timeout does NOT degrade the store.
func TestAuditorTimeoutDoesNotDegrade(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	// Install a seam that parks the audit pass beyond a short timeout.
	g := newGate()
	restore := SetAuditSeam(func(phase string) { g.hit() })
	defer func() { g.release(); restore() }()

	customAuditor := NewAuditor(s, AuditorOptions{
		Timeout: 30 * time.Millisecond,
	})
	s.SetAuditor(customAuditor)

	done := make(chan *AuditReport, 1)
	go func() {
		rep, _ := s.RunAudit(ctx, true)
		done <- rep
	}()

	g.waitEntered(t)
	// Let the timeout expire while parked.
	time.Sleep(50 * time.Millisecond)
	g.release()

	rep := <-done
	require.NotNil(t, rep)
	require.Equal(t, AuditResultTimeout, rep.LastResult, "timed-out audit must record timeout")
	require.Equal(t, StateOK, s.State(), "timeout must NOT degrade the store (§4.3)")
	require.Nil(t, s.DegradedError())
}

// 8. Explicit cancellation behavior.
func TestAuditorExplicitCancellation(t *testing.T) {
	s := seededStore(t, 3)

	g := newGate()
	restore := SetAuditSeam(func(phase string) { g.hit() })
	defer func() { g.release(); restore() }()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := s.RunAudit(ctx, true)
		errCh <- err
	}()

	g.waitEntered(t)
	cancel()
	g.release()

	err := <-errCh
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, StateOK, s.State(), "cancellation must NOT degrade the store")
}

// 9. Stable generation: concurrent writes do not cause false-positive omissions.
func TestAuditorStableGenerationUnderConcurrentWrites(t *testing.T) {
	s := seededStore(t, 10)
	ctx := context.Background()

	stop := make(chan struct{})
	var writeWg sync.WaitGroup

	// Run background writers while audit is executing.
	for i := 0; i < 3; i++ {
		writeWg.Add(1)
		go func(workerID int) {
			defer writeWg.Done()
			idx := 0
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.UpdateStatus(ctx, fmt.Sprintf("a-%d", idx%10), store.StatusWorking)
					idx++
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(i)
	}

	// Run audit during active mutations.
	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)

	close(stop)
	writeWg.Wait()

	t.Logf("audit findings: %+v", rep.Findings)
	require.Equal(t, AuditResultClean, rep.LastResult)
	require.Empty(t, rep.Findings)
	require.Equal(t, StateOK, s.State(), "store must remain healthy under concurrent writes")
}

// 10. Transition suspect -> ok on clean audit.
func TestAuditorTransitionSuspectToOK(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	s.TransitionSuspect()
	require.Equal(t, StateSuspect, s.State())

	rep, err := s.RunAudit(ctx, true)
	require.NoError(t, err)
	require.Equal(t, AuditResultClean, rep.LastResult)
	require.Equal(t, StateOK, s.State(), "clean audit must transition suspect back to ok")
}
