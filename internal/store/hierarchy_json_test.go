package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHierarchyAuthorityJSONAndDB(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"legacy", nil}, {"empty", []string{}}, {"ordered", []string{"z", "dangling", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := Session{ID: tc.name, ChildAgents: tc.ids, ChildPipelines: tc.ids, Subject: "preserved"}
			raw, err := json.Marshal(sess)
			require.NoError(t, err)
			if tc.ids == nil {
				require.NotContains(t, string(raw), `"child_agents"`)
			} else if len(tc.ids) == 0 {
				require.Contains(t, string(raw), `"child_agents":[]`)
				require.Contains(t, string(raw), `"child_pipelines":[]`)
			}
			var decoded Session
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, sess, decoded)
			dir := t.TempDir()
			s, err := NewFileStore(dir)
			require.NoError(t, err)
			require.NoError(t, s.Insert(ctx, &decoded))
			require.NoError(t, s.Close(ctx))
			s, err = NewFileStore(dir)
			require.NoError(t, err)
			defer s.Close(ctx)
			require.NoError(t, s.Update(ctx, sess.ID, func(s *Session) error { s.Subject = "changed"; return nil }))
			got, err := s.Get(ctx, sess.ID)
			require.NoError(t, err)
			require.Equal(t, tc.ids, got.ChildAgents)
			require.Equal(t, tc.ids, got.ChildPipelines)
		})
	}
}
