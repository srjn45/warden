package autopilot

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalPRTitle(t *testing.T) {
	cm := func(s string) CommitMsg { return CommitMsg{Subject: s} }
	long := strings.Repeat("word-", 30)
	tests := []struct {
		name    string
		plan    string
		commits []CommitMsg
		want    string
	}{
		{"feat wins over fix", "my-plan", []CommitMsg{cm("fix(a): x"), cm("feat(a): y")}, "feat(a): my plan"},
		{"shared scope of chosen type", "pipeline_cli_cleanup", []CommitMsg{cm("feat(pipeline): a"), cm("feat(pipeline): b"), cm("docs(pipeline): c")}, "feat(pipeline): pipeline cli cleanup"},
		{"scope falls back to all conventional", "p", []CommitMsg{cm("feat: a"), cm("feat: b"), cm("docs: c")}, "feat: p"},
		{"mixed scopes drop scope", "p", []CommitMsg{cm("feat(a): a"), cm("feat(b): b")}, "feat: p"},
		{"breaking marker", "p", []CommitMsg{cm("fix(a)!: a")}, "fix(a)!: p"},
		{"breaking footer", "p", []CommitMsg{{Subject: "docs: a", Body: "x\n\nBREAKING CHANGE: y"}}, "docs!: p"},
		{"no conventional commits", "p", []CommitMsg{cm("random thing")}, "chore: p"},
		{"no commits", "p", nil, "chore: p"},
		{"merge commits ignored", "p", []CommitMsg{cm("Merge pull request #1 from x/y"), cm("Merge branch 'main'")}, "chore: p"},
		{"rank order", "p", []CommitMsg{cm("ci: a"), cm("refactor: b"), cm("docs: c")}, "refactor: p"},
		{"long name truncated", long, []CommitMsg{cm("feat(pipeline): a")}, ""},
		{"empty name uses run id", "", []CommitMsg{cm("feat: a")}, "feat: ap-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FinalPRTitle(tc.plan, "ap-1", tc.commits)
			if tc.want == "" {
				require.LessOrEqual(t, len(got), 72)
				require.True(t, strings.HasPrefix(got, "feat(pipeline): word word"), got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCompletionOldStyleTitledFinalPRStillDetectedMerged(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	c.completionTick(context.Background())
	// The host holds an old "autopilot: <name>" PR; lookup is by number/head/base.
	rt.ensured[0].Title = "autopilot: legacy"
	rt.state.Gate = GateGreen
	c.completionTick(context.Background())
	require.Equal(t, StateAwaitingMerge, runState(c, id))
	rt.state = FinalPRState{State: "merged"}
	c.completionTick(context.Background())
	require.Equal(t, StateComplete, runState(c, id))
}
