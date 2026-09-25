package agentstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentHierarchyAuthorityDB(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ids  []string
	}{{"legacy", nil}, {"empty", []string{}}, {"ordered", []string{"z", "missing", "a"}}} {
		t.Run(tc.name, func(t *testing.T) {
			a := Agent{ID: tc.name, ChildAgents: tc.ids, ChildPipelines: tc.ids}
			raw, err := json.Marshal(a)
			require.NoError(t, err)
			var decoded Agent
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, a, decoded)
			dir := t.TempDir()
			s, err := New(dir)
			require.NoError(t, err)
			require.NoError(t, s.Insert(ctx, &decoded))
			require.NoError(t, s.Close())
			s, err = New(dir)
			require.NoError(t, err)
			defer s.Close()
			got, err := s.Get(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, tc.ids, got.ChildAgents)
			require.Equal(t, tc.ids, got.ChildPipelines)
		})
	}
}
