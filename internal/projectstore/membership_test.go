package projectstore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seedProject upserts a bare project and returns its id.
func seedProject(t *testing.T, s *Store, id string) string {
	t.Helper()
	require.NoError(t, s.Upsert(Project{ID: id, Name: id}))
	return id
}

func TestAddRemoveAgentToProject(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	// Add two agents, then a duplicate (idempotent no-op).
	p, err := s.AddAgentToProject(id, "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, p.Agents)
	p, err = s.AddAgentToProject(id, "a2")
	require.NoError(t, err)
	require.Equal(t, []string{"a1", "a2"}, p.Agents)
	p, err = s.AddAgentToProject(id, "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1", "a2"}, p.Agents)

	// Remove one, and removing an absent member is a no-op.
	p, err = s.RemoveAgentFromProject(id, "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, p.Agents)
	p, err = s.RemoveAgentFromProject(id, "zzz")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, p.Agents)

	// Persisted across a fresh Get.
	got, err := s.Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, got.Agents)
}

func TestAddRemovePipelineToProject(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	p, err := s.AddPipelineToProject(id, "pl1")
	require.NoError(t, err)
	require.Equal(t, []string{"pl1"}, p.Pipelines)
	p, err = s.AddPipelineToProject(id, "pl1")
	require.NoError(t, err)
	require.Equal(t, []string{"pl1"}, p.Pipelines)

	p, err = s.RemovePipelineFromProject(id, "pl1")
	require.NoError(t, err)
	require.Empty(t, p.Pipelines)
}

func TestAddRemoveTerminalToProject(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	p, err := s.AddTerminalToProject(id, "t1")
	require.NoError(t, err)
	require.Equal(t, []string{"t1"}, p.Terminals)
	p, err = s.AddTerminalToProject(id, "t2")
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t2"}, p.Terminals)
	// Idempotent.
	p, err = s.AddTerminalToProject(id, "t2")
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t2"}, p.Terminals)

	p, err = s.RemoveTerminalFromProject(id, "t1")
	require.NoError(t, err)
	require.Equal(t, []string{"t2"}, p.Terminals)
}

// The three lists are independent: adding an agent must not touch pipelines or
// terminals, and vice versa.
func TestMembershipListsAreIndependent(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	_, err := s.AddAgentToProject(id, "a1")
	require.NoError(t, err)
	_, err = s.AddPipelineToProject(id, "pl1")
	require.NoError(t, err)
	p, err := s.AddTerminalToProject(id, "t1")
	require.NoError(t, err)

	require.Equal(t, []string{"a1"}, p.Agents)
	require.Equal(t, []string{"pl1"}, p.Pipelines)
	require.Equal(t, []string{"t1"}, p.Terminals)
}

func TestMembershipErrors(t *testing.T) {
	s := newTestStore(t)

	// Missing project on every helper.
	for _, tc := range []struct {
		name string
		fn   func() (Project, error)
	}{
		{"add-agent", func() (Project, error) { return s.AddAgentToProject("nope", "a") }},
		{"remove-agent", func() (Project, error) { return s.RemoveAgentFromProject("nope", "a") }},
		{"add-pipeline", func() (Project, error) { return s.AddPipelineToProject("nope", "p") }},
		{"remove-pipeline", func() (Project, error) { return s.RemovePipelineFromProject("nope", "p") }},
		{"add-terminal", func() (Project, error) { return s.AddTerminalToProject("nope", "t") }},
		{"remove-terminal", func() (Project, error) { return s.RemoveTerminalFromProject("nope", "t") }},
	} {
		_, err := tc.fn()
		require.ErrorIsf(t, err, ErrNotFound, "%s should report ErrNotFound", tc.name)
	}

	// Blank member id on the Add helpers.
	id := seedProject(t, s, "p1")
	_, err := s.AddAgentToProject(id, "")
	require.ErrorIs(t, err, ErrInvalidID)
	_, err = s.AddPipelineToProject(id, "")
	require.ErrorIs(t, err, ErrInvalidID)
	_, err = s.AddTerminalToProject(id, "")
	require.ErrorIs(t, err, ErrInvalidID)
}

// Blank ids interleaved into an add are dropped by de-duplication.
func TestMembershipDedupesBlanksFromStoredList(t *testing.T) {
	s := newTestStore(t)
	// Seed a project whose Agents already carries a blank and a duplicate, as a
	// pre-field or hand-written record might.
	require.NoError(t, s.Upsert(Project{ID: "p1", Name: "p1", Agents: []string{"a1", "", "a1"}}))
	p, err := s.AddAgentToProject("p1", "a2")
	require.NoError(t, err)
	require.Equal(t, []string{"a1", "a2"}, p.Agents)
}

// A record written before the membership fields existed reads back with nil
// lists (omitempty) and the helpers still work on it.
func TestMembershipOmitEmptyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	got, err := s.Get(id)
	require.NoError(t, err)
	require.Nil(t, got.Agents)
	require.Nil(t, got.Pipelines)
	require.Nil(t, got.Terminals)

	p, err := s.AddAgentToProject(id, "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, p.Agents)
}
