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

func TestAddRemovePlanToProject(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	p, err := s.AddPlanToProject(id, "plan1")
	require.NoError(t, err)
	require.Equal(t, []string{"plan1"}, p.Plans)
	p, err = s.AddPlanToProject(id, "plan1")
	require.NoError(t, err)
	require.Equal(t, []string{"plan1"}, p.Plans)

	p, err = s.RemovePlanFromProject(id, "plan1")
	require.NoError(t, err)
	require.Empty(t, p.Plans)
	p, err = s.RemovePlanFromProject(id, "missing")
	require.NoError(t, err)
	require.Empty(t, p.Plans)
}

func TestAddRemoveAutopilotToProject(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")

	p, err := s.AddAutopilotToProject(id, "run1")
	require.NoError(t, err)
	require.Equal(t, []string{"run1"}, p.Autopilots)
	p, err = s.AddAutopilotToProject(id, "run1")
	require.NoError(t, err)
	require.Equal(t, []string{"run1"}, p.Autopilots)

	p, err = s.RemoveAutopilotFromProject(id, "run1")
	require.NoError(t, err)
	require.Empty(t, p.Autopilots)
	p, err = s.RemoveAutopilotFromProject(id, "missing")
	require.NoError(t, err)
	require.Empty(t, p.Autopilots)
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
		{"add-plan", func() (Project, error) { return s.AddPlanToProject("nope", "plan") }},
		{"remove-plan", func() (Project, error) { return s.RemovePlanFromProject("nope", "plan") }},
		{"add-autopilot", func() (Project, error) { return s.AddAutopilotToProject("nope", "run") }},
		{"remove-autopilot", func() (Project, error) { return s.RemoveAutopilotFromProject("nope", "run") }},
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
	_, err = s.AddPlanToProject(id, "")
	require.ErrorIs(t, err, ErrInvalidID)
	_, err = s.AddAutopilotToProject(id, "")
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
	require.Nil(t, got.Plans)
	require.Nil(t, got.Autopilots)

	p, err := s.AddAgentToProject(id, "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, p.Agents)
}

func TestReparentAgent(t *testing.T) {
	s := newTestStore(t)
	from := seedProject(t, s, "from")
	to := seedProject(t, s, "to")
	_, err := s.AddAgentToProject(from, "a1")
	require.NoError(t, err)
	_, err = s.AddAgentToProject(from, "a2")
	require.NoError(t, err)

	require.NoError(t, s.ReparentAgent("a1", from, to))
	gotFrom, err := s.Get(from)
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, gotFrom.Agents)
	gotTo, err := s.Get(to)
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, gotTo.Agents)

	// Idempotent when already on the target / same-project ensure.
	require.NoError(t, s.ReparentAgent("a1", from, to))
	gotTo, err = s.Get(to)
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, gotTo.Agents)
	require.NoError(t, s.ReparentAgent("a1", to, to))
	gotTo, err = s.Get(to)
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, gotTo.Agents)

	// Detach to project-less, then attach from empty source.
	require.NoError(t, s.ReparentAgent("a1", to, ""))
	gotTo, err = s.Get(to)
	require.NoError(t, err)
	require.Empty(t, gotTo.Agents)
	require.NoError(t, s.ReparentAgent("a1", "", from))
	gotFrom, err = s.Get(from)
	require.NoError(t, err)
	require.Equal(t, []string{"a2", "a1"}, gotFrom.Agents)

	// Missing source project is tolerated (dangling-id recovery).
	require.NoError(t, s.ReparentAgent("ghost", "missing-src", to))
	gotTo, err = s.Get(to)
	require.NoError(t, err)
	require.Equal(t, []string{"ghost"}, gotTo.Agents)

	// Missing target leaves the source untouched.
	require.ErrorIs(t, s.ReparentAgent("a2", from, "missing-dst"), ErrNotFound)
	gotFrom, err = s.Get(from)
	require.NoError(t, err)
	require.Equal(t, []string{"a2", "a1"}, gotFrom.Agents)

	require.ErrorIs(t, s.ReparentAgent("", from, to), ErrInvalidID)
}

func TestReparentPreservesOrderAcrossKinds(t *testing.T) {
	s := newTestStore(t)
	from := seedProject(t, s, "from")
	to := seedProject(t, s, "to")
	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := s.AddAgentToProject(from, id)
		require.NoError(t, err)
	}
	for _, id := range []string{"pl1", "pl2"} {
		_, err := s.AddPipelineToProject(from, id)
		require.NoError(t, err)
	}
	_, err := s.AddTerminalToProject(from, "t1")
	require.NoError(t, err)
	_, err = s.AddPlanToProject(from, "plan-1")
	require.NoError(t, err)
	_, err = s.AddPlanToProject(from, "plan-2")
	require.NoError(t, err)
	_, err = s.AddAutopilotToProject(from, "run-1")
	require.NoError(t, err)

	require.NoError(t, s.ReparentAgent("a2", from, to))
	require.NoError(t, s.ReparentPipeline("pl1", from, to))
	require.NoError(t, s.ReparentTerminal("t1", from, to))
	require.NoError(t, s.ReparentPlan("plan-2", from, to))
	require.NoError(t, s.ReparentAutopilot("run-1", from, to))

	gotFrom, err := s.Get(from)
	require.NoError(t, err)
	require.Equal(t, []string{"a1", "a3"}, gotFrom.Agents)
	require.Equal(t, []string{"pl2"}, gotFrom.Pipelines)
	require.Empty(t, gotFrom.Terminals)
	require.Equal(t, []string{"plan-1"}, gotFrom.Plans)
	require.Empty(t, gotFrom.Autopilots)

	gotTo, err := s.Get(to)
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, gotTo.Agents)
	require.Equal(t, []string{"pl1"}, gotTo.Pipelines)
	require.Equal(t, []string{"t1"}, gotTo.Terminals)
	require.Equal(t, []string{"plan-2"}, gotTo.Plans)
	require.Equal(t, []string{"run-1"}, gotTo.Autopilots)
}

// Dangling historical IDs survive add/remove/reparent and are never pruned.
func TestMembershipToleratesDanglingIDs(t *testing.T) {
	s := newTestStore(t)
	id := seedProject(t, s, "p1")
	require.NoError(t, s.Upsert(Project{
		ID: id, Name: id,
		Agents:     []string{"live", "ghost-agent"},
		Pipelines:  []string{"ghost-pipe"},
		Terminals:  []string{"ghost-term"},
		Plans:      []string{"ghost-plan", "live-plan"},
		Autopilots: []string{"ghost-run"},
	}))

	p, err := s.AddAgentToProject(id, "newer")
	require.NoError(t, err)
	require.Equal(t, []string{"live", "ghost-agent", "newer"}, p.Agents)
	p, err = s.RemovePlanFromProject(id, "live-plan")
	require.NoError(t, err)
	require.Equal(t, []string{"ghost-plan"}, p.Plans)
	require.Equal(t, []string{"ghost-pipe"}, p.Pipelines)
	require.Equal(t, []string{"ghost-term"}, p.Terminals)
	require.Equal(t, []string{"ghost-run"}, p.Autopilots)

	other := seedProject(t, s, "p2")
	require.NoError(t, s.ReparentAgent("ghost-agent", id, other))
	got, err := s.Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{"live", "newer"}, got.Agents)
	gotOther, err := s.Get(other)
	require.NoError(t, err)
	require.Equal(t, []string{"ghost-agent"}, gotOther.Agents)
}
