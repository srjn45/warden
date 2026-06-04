package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

// renderPipeline draws a pipeline's DAG in the detail pane when its header row is
// selected (mirrors renderApprovalsQueue). Read-only summary; actions come from
// keys handled by the model.
func renderPipeline(p *pipeline.Pipeline, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render("pipeline "+p.ID+" — "+string(p.Status)) + "\n\n")
	for i := range p.Jobs {
		j := &p.Jobs[i]
		line := fmt.Sprintf("%s %-12s %-13s", jobGlyph(j.Status), trunc(j.ID, 12), string(j.Status))
		if len(j.DependsOn) > 0 {
			line += stMuted.Render("deps: " + strings.Join(j.DependsOn, ","))
		}
		b.WriteString(line + "\n")
		if j.Output != "" {
			b.WriteString("    " + stMuted.Render(trunc(j.Output, max(0, width-4))) + "\n")
		}
	}
	b.WriteString("\n" + stMuted.Render("x cancel pipeline · on a job: r retry · a attach"))
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}
