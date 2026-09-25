package projectstore

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMembershipAuthorityJSONAndDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"legacy", nil}, {"empty", []string{}}, {"ordered", []string{"z", "missing", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Project{ID: tc.name, Name: "unchanged", Agents: tc.ids, Pipelines: tc.ids, Terminals: tc.ids}
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			if tc.ids == nil {
				require.NotContains(t, string(raw), `"agents"`)
			} else if len(tc.ids) == 0 {
				require.Contains(t, string(raw), `"agents":[]`)
			}
			var decoded Project
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, p, decoded)
			dir := t.TempDir()
			s, err := NewStore(dir)
			require.NoError(t, err)
			require.NoError(t, s.Upsert(decoded))
			require.NoError(t, s.Close())
			s, err = NewStore(dir)
			require.NoError(t, err)
			defer s.Close()
			got, err := s.Get(p.ID)
			require.NoError(t, err)
			require.Equal(t, tc.ids, got.Agents)
			require.Equal(t, tc.ids, got.Pipelines)
			require.Equal(t, tc.ids, got.Terminals)
			require.NoError(t, s.Upsert(Project{ID: p.ID, Name: "metadata update"}))
			got, err = s.Get(p.ID)
			require.NoError(t, err)
			require.Equal(t, tc.ids, got.Agents)
			require.Equal(t, tc.ids, got.Pipelines)
			require.Equal(t, tc.ids, got.Terminals)
		})
	}
}

func TestNewProjectAndLastRemovalStayAuthoritative(t *testing.T) {
	s := newTestStore(t)
	p, err := s.OpenProject("p", "p", "")
	require.NoError(t, err)
	require.Equal(t, []string{}, p.Agents)
	require.Equal(t, []string{}, p.Pipelines)
	require.Equal(t, []string{}, p.Terminals)
	_, err = s.AddAgentToProject("p", "a")
	require.NoError(t, err)
	_, err = s.AddPipelineToProject("p", "pipe")
	require.NoError(t, err)
	_, err = s.AddTerminalToProject("p", "t")
	require.NoError(t, err)
	_, err = s.RemoveAgentFromProject("p", "a")
	require.NoError(t, err)
	_, err = s.RemovePipelineFromProject("p", "pipe")
	require.NoError(t, err)
	_, err = s.RemoveTerminalFromProject("p", "t")
	require.NoError(t, err)
	p, err = s.Get("p")
	require.NoError(t, err)
	require.Equal(t, []string{}, p.Agents)
	require.Equal(t, []string{}, p.Pipelines)
	require.Equal(t, []string{}, p.Terminals)
}
