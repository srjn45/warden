package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/digest"
)

// renderDigest renders the digest detail body: a loading placeholder, an error,
// or the summary + file list + meta line. Pure — width is the inner pane width.
func renderDigest(d *digest.Digest, loading bool, err error, width int) string {
	if loading {
		return stMuted.Render("generating digest…")
	}
	if err != nil {
		return stError.Render("digest failed: " + err.Error())
	}
	if d == nil {
		return stMuted.Render("press d to generate a digest")
	}
	var b strings.Builder
	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if len(d.Files) == 0 {
		b.WriteString(stMuted.Render("(no files touched)"))
	} else {
		for _, f := range d.Files {
			mark := " "
			if f.Edited {
				mark = "*"
			}
			fmt.Fprintf(&b, "%s %s  +%d -%d\n", mark, f.Path, f.Added, f.Removed)
		}
	}
	branch := d.Branch
	if branch == "" {
		branch = "—"
	}
	fmt.Fprintf(&b, "\n%s", stMuted.Render(fmt.Sprintf("branch: %s · turns: %d · status: %s", branch, d.Turns, d.Status)))
	return b.String()
}
