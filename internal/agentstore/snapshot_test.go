package agentstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// TestSnapshotIsolation verifies that mutating returned *Agent records
// from Get, List, or GetByNameOrID does not mutate the published snapshot (Invariant I-9).
func TestSnapshotIsolation(t *testing.T) {
	s := seededStore(t, 3)
	ctx := context.Background()

	// 1. Get isolation
	a, err := s.Get(ctx, "a-0")
	require.NoError(t, err)
	a.Status = store.StatusDone
	a.Tags = append(a.Tags, "mutated")
	if len(a.Events) > 0 {
		a.Events[0].Detail = "mutated"
	}

	snap := s.Snapshot()
	require.NotNil(t, snap)
	storedA, err := snap.Get("a-0")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, storedA.Status, "snapshot status must be untouched")
	require.NotContains(t, storedA.Tags, "mutated", "snapshot tags must be untouched")

	// 2. List isolation
	list := snap.List()
	require.Len(t, list, 3)
	list[0].Status = store.StatusDone

	list2 := s.Snapshot().List()
	require.Equal(t, store.StatusWorking, list2[0].Status)
}

// TestSnapshotArchiveSingleSwap verifies Invariant I-7: Archive moves a row
// between active and closed in a single atomic snapshot swap.
func TestSnapshotArchiveSingleSwap(t *testing.T) {
	s := seededStore(t, 2)
	ctx := context.Background()

	snapBefore := s.Snapshot()
	require.Equal(t, uint64(3), snapBefore.Version) // 1 initial + 2 inserts

	_, err := snapBefore.Get("a-0")
	require.NoError(t, err)

	require.NoError(t, s.Archive(ctx, "a-0"))

	snapAfter := s.Snapshot()
	require.Equal(t, uint64(4), snapAfter.Version, "Archive must bump version monotonically (I-8)")

	// In the new snapshot, a-0 is gone from active and present in closed
	_, err = snapAfter.Get("a-0")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, snapAfter.ByID["a-0"])
	require.Nil(t, snapAfter.ByName["n-0"])

	require.NotNil(t, snapAfter.ClosedByID["a-0"])
	closedList := snapAfter.ListClosed()
	require.Len(t, closedList, 1)
	require.Equal(t, "a-0", closedList[0].ID)

	// An old reader observing snapBefore still sees a consistent previous view
	aOld, err := snapBefore.Get("a-0")
	require.NoError(t, err)
	require.Equal(t, "a-0", aOld.ID)
}

// TestSnapshotReadYourWrites verifies Invariant I-8: read-your-writes and monotonic versions.
func TestSnapshotReadYourWrites(t *testing.T) {
	s := seededStore(t, 1)
	ctx := context.Background()

	snap0 := s.Snapshot()
	v0 := snap0.Version

	// Insert
	require.NoError(t, s.Insert(ctx, &Agent{ID: "new-1", Name: "name-1", Status: store.StatusWorking}))
	snap1 := s.Snapshot()
	require.Greater(t, snap1.Version, v0)
	got, err := s.Get(ctx, "new-1")
	require.NoError(t, err)
	require.Equal(t, "new-1", got.ID)

	// Update
	require.NoError(t, s.UpdateStatus(ctx, "new-1", store.StatusIdle))
	snap2 := s.Snapshot()
	require.Greater(t, snap2.Version, snap1.Version)
	got, err = s.Get(ctx, "new-1")
	require.NoError(t, err)
	require.Equal(t, store.StatusIdle, got.Status)

	// Delete
	require.NoError(t, s.Delete(ctx, "new-1"))
	snap3 := s.Snapshot()
	require.Greater(t, snap3.Version, snap2.Version)
	_, err = s.Get(ctx, "new-1")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSnapshotMutationFailureDoesNotCorruptSnapshot verifies that failed mutations
// leave the published snapshot completely untouched.
func TestSnapshotMutationFailureDoesNotCorruptSnapshot(t *testing.T) {
	s := seededStore(t, 2)
	ctx := context.Background()

	snapBefore := s.Snapshot()
	vBefore := snapBefore.Version

	// 1. Duplicate ID Insert fails
	err := s.Insert(ctx, &Agent{ID: "a-0", Name: "unique-name"})
	require.ErrorIs(t, err, ErrExists)
	require.Equal(t, vBefore, s.Snapshot().Version)

	// 2. Duplicate Name Insert fails
	err = s.Insert(ctx, &Agent{ID: "unique-id", Name: "n-0"})
	require.ErrorIs(t, err, ErrNameExists)
	require.Equal(t, vBefore, s.Snapshot().Version)

	// 3. Update callback error fails
	err = s.Update(ctx, "a-0", func(a *Agent) error {
		return fmt.Errorf("intentional failure")
	})
	require.Error(t, err)
	require.Equal(t, vBefore, s.Snapshot().Version)

	// 4. Delete missing agent fails
	err = s.Delete(ctx, "nonexistent")
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, vBefore, s.Snapshot().Version)
}

// TestSnapshotConcurrentReadersAndWriters performs concurrent reads and writes
// to verify race freedom and snapshot consistency under load.
func TestSnapshotConcurrentReadersAndWriters(t *testing.T) {
	s := seededStore(t, 10)
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 4 reader goroutines
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var lastVersion uint64
			for {
				select {
				case <-stop:
					return
				default:
					snap := s.Snapshot()
					require.NotNil(t, snap)
					require.GreaterOrEqual(t, snap.Version, lastVersion, "versions must be monotonic")
					lastVersion = snap.Version

					list, err := s.List(ctx)
					require.NoError(t, err)
					require.NotEmpty(t, list)

					// Point lookup
					targetID := fmt.Sprintf("a-%d", id%5)
					_, _ = s.Get(ctx, targetID)
					_, _ = s.GetByNameOrID(ctx, fmt.Sprintf("n-%d", id%5))
				}
			}
		}(i)
	}

	// 2 writer goroutines
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				agentID := fmt.Sprintf("a-%d", (writerID*5+j)%10)
				status := store.StatusIdle
				if j%2 == 0 {
					status = store.StatusWorking
				}
				_ = s.UpdateStatus(ctx, agentID, status)
				time.Sleep(2 * time.Millisecond)
			}
		}(i)
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
