package autopilot

// COMPATIBILITY SUITE — autopilot worker/fix prompts (task t1-compat-baseline).
//
// Workers and the fix loop are steered by these exact instruction strings; the
// worker tooling (wd sync/check/job done) is named verbatim in them. They are
// pinned whole so a git/check CLI change cannot silently drift the prompts.
//
// Later tasks may only change an assertion in this file when their task prompt
// says so, and must say why in the commit message.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompatWorkerSpawnBranchPrompt(t *testing.T) {
	require.Equal(t,
		"wd sync onto autopilot/integration-x first. wd check before PR. wd job done when green. Open PRs against autopilot/integration-x — never main.",
		WorkerSpawnBranchPrompt("autopilot/integration-x"))
	require.Equal(t,
		"wd sync onto b first. wd check before PR. wd job done when green. Open PRs against b — never main.",
		WorkerSpawnBranchPrompt("  b \n"), "branch is trimmed")
	require.Empty(t, WorkerSpawnBranchPrompt(""))
	require.Empty(t, WorkerSpawnBranchPrompt("   "))
}

func TestCompatAppendWorkerSpawnBranch(t *testing.T) {
	const hint = "wd sync onto br first. wd check before PR. wd job done when green. Open PRs against br — never main."
	require.Equal(t, "do it\n\n"+hint, AppendWorkerSpawnBranch("do it", "br"))
	require.Equal(t, "do it\n\n"+hint, AppendWorkerSpawnBranch("  do it\n", "br"), "prompt is trimmed before appending")
	require.Equal(t, hint, AppendWorkerSpawnBranch("", "br"))
	require.Equal(t, hint, AppendWorkerSpawnBranch("  ", "br"))
	require.Equal(t, "already on br", AppendWorkerSpawnBranch("already on br", "br"), "no duplicate when branch already mentioned")
	require.Equal(t, "prompt", AppendWorkerSpawnBranch("prompt", ""), "empty branch is a no-op")
}

func TestCompatFixMessageGolden(t *testing.T) {
	pr := OpenPR{Number: 42, HeadRef: "feat/x", HeadSHA: "0123456789abcdef"}

	red := LandFix{PR: pr, Kind: FixRed, Detail: "CI is red"}
	ev := composeFixEvidence(red, FixEvidence{Jobs: []string{"build", "test"}, Log: "boom"})
	require.Equal(t, "Failing CI jobs: build, test\nCI is red\n\nFailed-log excerpt:\nboom\n", ev)
	require.Equal(t,
		"The PR #42 (branch feat/x, head 01234567) is not mergeable.\n\n"+ev+"\n"+
			"Fix the failure on this branch, run `wd check`, and push."+
			"\nWhen it is pushed and locally green, end with `wd job done`. Do NOT merge the PR — autopilot lands it.\n",
		composeFixMessage(red, ev))

	conflict := LandFix{PR: pr, Kind: FixConflict, Detail: "conflicts in a.go"}
	cev := composeFixEvidence(conflict, FixEvidence{})
	require.Equal(t, "The PR conflicts with its base branch.\nconflicts in a.go\n", cev)
	require.Equal(t,
		"The PR #42 (branch feat/x, head 01234567) is not mergeable.\n\n"+cev+"\n"+
			"Merge or rebase the PR's base branch into your branch, resolve the conflicts preserving both sides' intent, re-run `wd check`, and push."+
			"\nWhen it is pushed and locally green, end with `wd job done`. Do NOT merge the PR — autopilot lands it.\n",
		composeFixMessage(conflict, cev))
}

func TestCompatFixEvidenceTruncation(t *testing.T) {
	out := composeFixEvidence(LandFix{Kind: FixRed}, FixEvidence{Log: strings.Repeat("x", 3*fixEvidenceByteLimit)})
	require.Equal(t, fixEvidenceByteLimit+len("\n…(truncated)"), len(out))
	require.True(t, strings.HasSuffix(out, "\n…(truncated)"))
	require.Equal(t, 8<<10, fixEvidenceByteLimit)
}

func TestCompatShortSHA(t *testing.T) {
	require.Equal(t, "01234567", short("0123456789"))
	require.Equal(t, "01234567", short("01234567"))
	require.Equal(t, "abc", short("abc"))
}
