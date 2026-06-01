package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFakeRunnerRecordsCalls(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git status --porcelain": {Out: ""},
	}}
	out, err := fr.Run(context.Background(), "", "git", "status", "--porcelain")
	require.NoError(t, err)
	require.Equal(t, "", out)
	require.Len(t, fr.Calls, 1)
	require.Equal(t, []string{"git", "status", "--porcelain"}, fr.Calls[0].Argv)
}

func TestFakeRunnerKeyMatch(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1": {Err: errStub("no session")},
	}}
	_, err := fr.Run(context.Background(), "", "tmux", "has-session", "-t", "A-1")
	require.Error(t, err)
}

type errStub string

func (e errStub) Error() string { return string(e) }
