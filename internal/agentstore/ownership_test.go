package agentstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestOwnershipAliasesAndTypedError(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir)
	require.NoError(t, err)

	link := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(dir, link))
	wd, _ := os.Getwd()
	rel, err := filepath.Rel(wd, dir)
	require.NoError(t, err)

	for _, alias := range []string{link, rel, dir + "/./"} {
		_, err := New(alias)
		require.ErrorIs(t, err, store.ErrStoreOwned, alias)
		var oe *OwnershipError
		require.True(t, errors.As(err, &oe))
		require.Contains(t, oe.NextStep(), "stop the running warden daemon")
	}

	require.NoError(t, first.Close())
	again, err := New(link)
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

func TestOwnershipRejectedOpenerDoesNotWipe(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir)
	require.NoError(t, err)
	defer first.Close()
	// Even without marker, opener must not wipe agents-db.
	_ = os.Remove(filepath.Join(dir, importedMarker))
	_, err = New(dir)
	require.ErrorIs(t, err, store.ErrStoreOwned)
	require.DirExists(t, filepath.Join(dir, "agents-db"))
}
