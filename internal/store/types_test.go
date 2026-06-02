package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSessionBSONRoundTrip(t *testing.T) {
	s := Session{
		ID:          "PROJ-350",
		Type:        TypeDevelopment,
		Ticket:      "PROJ-350",
		TmuxSession: "PROJ-350",
		Repo:        "/repo",
		Worktree:    ".worktrees/PROJ-350",
		Branch:      "PROJ-350",
		Prompt:      "do a security review of the auth module",
		Status:      StatusSpawning,
		PID:         123,
		Events:      []Event{{Type: "SessionStart"}},
	}
	raw, err := bson.Marshal(s)
	require.NoError(t, err)

	var got Session
	require.NoError(t, bson.Unmarshal(raw, &got))
	require.Equal(t, "PROJ-350", got.ID)
	require.Equal(t, TypeDevelopment, got.Type)
	require.Equal(t, StatusSpawning, got.Status)
	require.Len(t, got.Events, 1)
	require.Equal(t, "SessionStart", got.Events[0].Type)
	require.Equal(t, "do a security review of the auth module", got.Prompt)
}

func TestStatusValid(t *testing.T) {
	require.True(t, StatusWorking.Valid())
	require.False(t, Status("bogus").Valid())
}

func TestTypeNormalizeAndWorktreePolicy(t *testing.T) {
	// Known types keep their value; unknown collapses to "other".
	require.Equal(t, TypeDevelopment, NormalizeType("development"))
	require.Equal(t, TypeOther, NormalizeType("totally-made-up"))

	// Default worktree policy per design §2.
	require.True(t, TypeDevelopment.DefaultWorktree())
	require.True(t, TypePRReview.DefaultWorktree())
	require.False(t, TypeBuildkiteDebug.DefaultWorktree())
	require.False(t, TypeSpike.DefaultWorktree()) // opt-in via --worktree, not default
}
