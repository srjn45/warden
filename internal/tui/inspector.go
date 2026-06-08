package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/warden/internal/client"
)

// The inspector is the cockpit's read-only window onto the daemon's shared
// state: the namespaced context KV store agents write to, and recent directed
// message traffic between agents. It is rendered into the list pane on `c` and
// owns no mutations — purely a viewer (edit/send happen via the CLI/web).

// oneLine collapses any run of whitespace (including newlines) into single
// spaces so a multi-line stored value renders on one row.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ctxLine renders one context entry as "key  value", with the value flattened to
// a single line and truncated to valWidth.
func ctxLine(e client.ContextEntry, valWidth int) string {
	return e.Key + "  " + stMuted.Render(trunc(oneLine(e.Value), valWidth))
}

// msgLine renders one message as "from → to  body", body flattened + truncated.
func msgLine(m client.Message, bodyWidth int) string {
	return fmt.Sprintf("%s → %s  %s", m.From, m.To, trunc(oneLine(m.Body), bodyWidth))
}

// inspectorBody composes the two read-only sections (shared context, then recent
// messages) into the pane body text. width bounds value/body truncation.
func inspectorBody(entries []client.ContextEntry, msgs []client.Message, width int) string {
	// Reserve room for the "key  " / "from → to  " prefixes; never go below a
	// small floor so narrow panes still show something.
	valWidth := max(8, width-24)

	var b strings.Builder
	b.WriteString(stHeader.Render(fmt.Sprintf("Shared context (%d)", len(entries))))
	b.WriteString("\n")
	if len(entries) == 0 {
		b.WriteString(stMuted.Render("  no shared context yet"))
		b.WriteString("\n")
	} else {
		for _, e := range entries {
			b.WriteString("  " + ctxLine(e, valWidth) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(stHeader.Render(fmt.Sprintf("Messages (%d)", len(msgs))))
	b.WriteString("\n")
	if len(msgs) == 0 {
		b.WriteString(stMuted.Render("  no messages yet"))
	} else {
		for _, m := range msgs {
			b.WriteString("  " + msgLine(m, valWidth) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
