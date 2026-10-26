package planstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

// List and ListByProject read through indexes; they must still return every
// live plan exactly once at its latest revision, however often it was rewritten.
func TestListUsesLiveRecordsAcrossRewrites(t *testing.T) {
	ctx := context.Background()
	s, db := newSchema3PlanFixture(t)
	projects, err := wardendb.Lookup[map[string]any](db, "projects")
	require.NoError(t, err)
	col, err := projects.Engine()
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("q", map[string]any{"id": "q", "name": "other", "status": "open", "path": ""})
	require.NoError(t, err)

	for i, owner := range []string{"p", "p", "q"} {
		p := &Plan{ID: fmt.Sprintf("plan-%d", i), ProjectID: owner, Name: "n", Goal: "g", Tasks: []PlanTask{{ID: "t", Prompt: "do"}}, Status: PlanStatusPending}
		require.NoError(t, s.Create(ctx, p))
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, s.Update(ctx, "plan-0", func(p *Plan) error { p.Goal = fmt.Sprintf("goal %d", i); RefreshContentHash(p); return nil }))
	}

	all, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for _, p := range all {
		if p.ID == "plan-0" {
			require.Equal(t, "goal 19", p.Goal, "latest revision")
		}
	}
	inP, err := s.ListByProject(ctx, "p")
	require.NoError(t, err)
	require.Len(t, inP, 2)
	inQ, err := s.ListByProject(ctx, "q")
	require.NoError(t, err)
	require.Len(t, inQ, 1)
	require.Equal(t, "plan-2", inQ[0].ID)
	none, err := s.ListByProject(ctx, "missing")
	require.NoError(t, err)
	require.Empty(t, none)
}
