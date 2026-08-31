package store

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStoreOwnershipRejectsSecondWriter verifies the single-owner invariant: once
// one process holds the writable store, a second NewFileStore on the same data dir
// fails with ErrStoreOwned instead of opening a racing second writer (the exact
// corruption source this work closes).
func TestStoreOwnershipRejectsSecondWriter(t *testing.T) {
	dir := t.TempDir()

	first, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close(context.Background()) })

	second, err := NewFileStore(dir)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrStoreOwned)
}

// TestStoreOwnershipReleasedOnClose verifies the lock is held for the store's
// lifetime and released on Close, so a legitimate handoff (daemon stops, offline
// tool opens) succeeds after the owner closes.
func TestStoreOwnershipReleasedOnClose(t *testing.T) {
	dir := t.TempDir()

	first, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, first.Close(context.Background()))

	second, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
}

// TestStoreOwnershipDoesNotWipeOnRejection is the safety guarantee behind taking
// the lock BEFORE the import-wipe: a rejected second opener must not have touched
// (reset/wiped/written) the live store. Data inserted through the first store
// survives the failed second-open and a clean reopen.
func TestStoreOwnershipDoesNotWipeOnRejection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, first.Insert(ctx, sample()))

	// Second opener is rejected while the first still holds the store.
	_, err = NewFileStore(dir)
	require.ErrorIs(t, err, ErrStoreOwned)

	require.NoError(t, first.Close(ctx))

	reopened, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close(ctx) })

	got, err := reopened.Get(ctx, sample().ID)
	require.NoError(t, err, "the rejected second open must not have wiped the store")
	require.Equal(t, sample().ID, got.ID)
}

// TestStoreOwnershipConcurrentOpeners hammers a held store with many concurrent
// open attempts: every one must fail with ErrStoreOwned (never a silent success
// that would mean two live writers).
func TestStoreOwnershipConcurrentOpeners(t *testing.T) {
	dir := t.TempDir()

	owner, err := NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(context.Background()) })

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	stores := make([]*FileStore, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = NewFileStore(dir)
		}(i)
	}
	wg.Wait()

	for i := range n {
		require.ErrorIs(t, errs[i], ErrStoreOwned, "opener %d must be rejected", i)
		require.Nil(t, stores[i], "opener %d must not have obtained a store", i)
	}
}
