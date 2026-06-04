package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/approval"
)

// parseOption parses a 1-based option argument; rejects non-integers and < 1.
func parseOption(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("option must be a positive integer, got %q", s)
	}
	return n, nil
}

// validateApproval finds the approval for id in the live queue and checks the
// option is answerable. Returns friendly errors the daemon would otherwise only
// surface after a round-trip. The daemon still re-verifies on POST.
func validateApproval(views []approval.View, id string, option int) (approval.View, error) {
	for _, v := range views {
		if v.ID != id {
			continue
		}
		if !v.Recognized {
			return approval.View{}, fmt.Errorf("prompt for %s is not a recognized menu — attach with: agentctl attach %s", id, id)
		}
		if option < 1 || option > len(v.Options) {
			return approval.View{}, fmt.Errorf("option %d out of range (1-%d)", option, len(v.Options))
		}
		return v, nil
	}
	return approval.View{}, fmt.Errorf("no pending approval for %s (run: agentctl approvals)", id)
}

// formatApprovalsList renders the queue. Recognized prompts are shown with their
// numbered options and an answer hint; unrecognized waiting agents are summarized
// in a footer (they must be attached, not answered here).
func formatApprovalsList(enabled bool, views []approval.View) string {
	if !enabled {
		return "approvals disabled (set AGENTCTL_APPROVALS=on)\n"
	}
	var b strings.Builder
	recognized, unrecognized := 0, 0
	for _, v := range views {
		if v.Recognized {
			recognized++
			b.WriteString(formatApproval(v))
		} else {
			unrecognized++
		}
	}
	out := b.String()
	if recognized == 0 {
		out = "(no pending approvals)\n"
	}
	if unrecognized > 0 {
		out += fmt.Sprintf("(%d other waiting agent(s) need attaching — not answerable here)\n", unrecognized)
	}
	return out
}

// formatApproval renders one recognized prompt block.
func formatApproval(v approval.View) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", v.ID, v.Question)
	for i, opt := range v.Options {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, opt)
	}
	fmt.Fprintf(&b, "  answer: agentctl approve %s <n>\n", v.ID)
	return b.String()
}

var _ = cobra.Command{} // keep cobra imported for Task 2; remove if Task 2 lands in same change
