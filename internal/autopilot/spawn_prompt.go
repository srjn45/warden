package autopilot

import "strings"

// WorkerSpawnBranchPrompt is the instruction every autopilot worker spawn must
// carry so the worker bases PRs on the resolved per-plan integration branch
// instead of guessing `autopilot/integration` or main.
func WorkerSpawnBranchPrompt(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return "wd sync onto " + branch + " first. wd check before PR. wd job done when green. Open PRs against " + branch + " — never main."
}

// AppendWorkerSpawnBranch appends WorkerSpawnBranchPrompt to prompt when the
// branch is not already mentioned. Empty branch is a no-op.
func AppendWorkerSpawnBranch(prompt, branch string) string {
	hint := WorkerSpawnBranchPrompt(branch)
	if hint == "" {
		return prompt
	}
	if strings.Contains(prompt, branch) {
		return prompt
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return hint
	}
	return prompt + "\n\n" + hint
}
