package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreatePROpensPR(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"gh pr create --base main --head feature-x --title My title --body Body text": {
			Out: "https://github.com/o/r/pull/7\n",
		},
	}}
	res, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "My title", "Body text", "")
	require.NoError(t, err)
	require.True(t, res.Created)
	require.Equal(t, "feature-x", res.Branch)
	require.Equal(t, "main", res.Base, "empty base defaults to main")
	require.Equal(t, "https://github.com/o/r/pull/7", res.URL)
}

func TestCreatePRUsesSuppliedBase(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"gh pr create --base develop --head feature-x --title T --body B": {
			Out: "https://github.com/o/r/pull/9\n",
		},
	}}
	res, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "T", "B", "develop")
	require.NoError(t, err)
	require.Equal(t, "develop", res.Base)
	require.Equal(t, "https://github.com/o/r/pull/9", res.URL)
}

func TestCreatePRExistingIsNotAnError(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"gh pr create --base main --head feature-x --title T --body B": {
			Out: "a pull request for branch \"feature-x\" into branch \"main\" already exists:\nhttps://github.com/o/r/pull/3",
			Err: errors.New("exit status 1"),
		},
	}}
	res, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "T", "B", "")
	require.NoError(t, err, "an already-existing PR is a result, not an error")
	require.False(t, res.Created)
	require.Equal(t, "https://github.com/o/r/pull/3", res.URL)
}

func TestCreatePRRefusesProtectedBranch(t *testing.T) {
	for _, b := range []string{"main", "master"} {
		fr := &FakeRunner{Responses: map[string]FakeResp{
			"git rev-parse --abbrev-ref HEAD": {Out: b + "\n"},
		}}
		_, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "T", "B", "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "protected branch")
		// gh must never run for a protected head branch.
		for _, c := range fr.Calls {
			require.NotEqual(t, "gh", c.Argv[0], "gh should not be invoked on a protected branch")
		}
	}
}

func TestCreatePRNotARepo(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Err: errors.New("not a repo")},
	}}
	_, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "T", "B", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a git repository")
}

func TestCreatePRGhFailureSurfaces(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature-x\n"},
		"gh pr create --base main --head feature-x --title T --body B": {
			Out: "could not determine base repo",
			Err: errors.New("exit status 1"),
		},
	}}
	_, err := New(fr, &FakeConfig{}).CreatePR(context.Background(), "/wt", "T", "B", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "gh pr create")
}
