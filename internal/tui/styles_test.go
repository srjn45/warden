package tui

import (
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestBadgeErroredShowsExitCode(t *testing.T) {
	code := 137
	label, _ := badge(store.StatusErrored, &code)
	require.Equal(t, "error 137", label)

	label2, _ := badge(store.StatusErrored, nil)
	require.Equal(t, "error", label2)

	label3, _ := badge(store.StatusDone, nil)
	require.Equal(t, "done", label3)
}
