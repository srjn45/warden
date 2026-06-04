package pipeline

import (
	"fmt"
	"strings"
)

// ComposePrompt builds the full prompt the daemon types into a job's agent:
// upstream outputs (for dependencies that produced one) + the job's own prompt +
// a footer telling it how to emit its handoff.
func ComposePrompt(p *Pipeline, job *Job) string {
	var b strings.Builder
	for _, dep := range job.DependsOn {
		up := p.Job(dep)
		if up == nil || up.Output == "" {
			continue
		}
		fmt.Fprintf(&b, "### Upstream output — job `%s`:\n%s\n", dep, up.Output)
		if up.Branch != "" {
			fmt.Fprintf(&b, "(branch: `%s`)\n", up.Branch)
		}
		b.WriteString("\n")
	}
	b.WriteString(job.Prompt)
	fmt.Fprintf(&b, "\n\n---\nYou are job `%s` in pipeline `%s`. When your task is complete, "+
		"publish your handoff for downstream jobs by running:\n"+
		"  agentctl pipeline emit \"<your handoff text>\"\n", job.ID, p.ID)
	if strings.TrimSpace(job.Handoff) != "" {
		fmt.Fprintf(&b, "Include specifically: %s\n", job.Handoff)
	}
	return b.String()
}
