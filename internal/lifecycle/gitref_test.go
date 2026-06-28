package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeGitRef(t *testing.T) {
	// Allowed: empty (callers default it) and any ref not starting with '-'.
	for _, ok := range []string{"", "main", "feature/foo", "release-1.2", "v1.0.0", "a-b", "x.y_z"} {
		require.NoError(t, safeGitRef(ok), "ref %q should be allowed", ok)
	}
	// Rejected: option-like values that git/gh would parse as a flag.
	for _, bad := range []string{"-", "--upload-pack=touch /tmp/pwned", "--exec=sh", "-x"} {
		require.Error(t, safeGitRef(bad), "ref %q should be rejected", bad)
	}
}

func TestSyncRejectsOptionLikeBaseBeforeFetch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
	}}
	_, err := New(fr, &FakeConfig{}).Sync(context.Background(), "/wt", "--upload-pack=touch /tmp/pwned")
	require.Error(t, err)
	// The injection must be caught before any fetch/rebase is attempted.
	for _, call := range fr.calledArgs() {
		require.NotEqual(t, "fetch", call[1], "no git fetch may run for a rejected base")
	}
}
