package repl

import (
	"fmt"
	"io"
	"strings"
)

// suggestMax bounds the live `/`-command panel so it never floods the screen
// (and so the cursor save/restore stays inside the visible region).
const suggestMax = 8

// suggestion is one row of the live `/`-command panel: the command's usage line
// (verb + arg hint) and its one-line summary.
type suggestion struct {
	usage   string
	summary string
}

// suggestMatches returns the deterministic commands whose name or an alias
// begins with the slash word the operator is currently typing. It fires only
// while the *first* token is still being typed — a leading "/" with no space
// yet. Once the operator moves on to arguments (a space appears) the panel gets
// out of the way, since the verb is already chosen. A bare "/" matches every
// command, so typing just "/" reveals the whole menu (Claude-Code style).
func suggestMatches(line string) []suggestion {
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \t") {
		return nil
	}
	var out []suggestion
	for _, c := range commandList {
		if matchesPrefix(c, line) {
			out = append(out, suggestion{usage: c.usage, summary: c.summary})
		}
	}
	// /help is a virtual verb (not in commandList) but is worth surfacing too.
	if strings.HasPrefix("/help", line) {
		out = append(out, suggestion{usage: "/help", summary: "list every command"})
	}
	return out
}

// matchesPrefix reports whether the command's canonical name or any alias starts
// with the typed prefix, so "/sp" surfaces /spawn and "/ls" surfaces /agents.
func matchesPrefix(c command, prefix string) bool {
	if strings.HasPrefix(c.name, prefix) {
		return true
	}
	for _, a := range c.aliases {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// formatSuggestions renders the panel body: one "  <usage>  <summary>" row per
// match, the verb in the tool colour and the summary dimmed. It returns "" when
// there is nothing to show. Rows are newline-joined; the caller turns those into
// raw-mode "\r\n" when it paints under the live editor. Plain (no ANSI) when the
// styler's writer is not a terminal, which keeps it assertion-friendly in tests.
func formatSuggestions(matches []suggestion, st *styler) string {
	if len(matches) == 0 {
		return ""
	}
	more := 0
	if len(matches) > suggestMax {
		more = len(matches) - suggestMax
		matches = matches[:suggestMax]
	}
	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("  " + st.tool.Render(fmt.Sprintf("%-26s", m.usage)) + " " + st.hint.Render(m.summary))
	}
	if more > 0 {
		b.WriteString("\n  " + st.hint.Render(fmt.Sprintf("…and %d more — keep typing to narrow", more)))
	}
	return b.String()
}

// suggester is the readline Listener that paints the live `/`-command panel
// beneath the prompt as the operator types — warden's take on Claude Code's
// slash-command menu. It writes to the same terminal readline draws on; since
// OnChange is called synchronously inside readline's single read loop, the two
// never race. The panel is purely visual: OnChange never rewrites the buffer.
type suggester struct {
	out   io.Writer
	style *styler
	shown bool // a panel is currently painted below the line
}

// OnChange repaints (or clears) the panel for each keystroke. It bows out of the
// way when readline owns the area below the line itself — Tab completion and
// Ctrl-R/Ctrl-S search — and wipes the panel cleanly on submit/cancel so no rows
// are orphaned above the next output.
func (sg *suggester) OnChange(line []rune, _ int, key rune) ([]rune, int, bool) {
	switch key {
	case '\r', '\n': // submit: the cursor already sits on the line below the
		// input (readline wrote the trailing newline), so clear straight down.
		if sg.shown {
			io.WriteString(sg.out, "\033[0J")
			sg.shown = false
		}
	case '\t', 0x12, 0x13, 0x03: // Tab / Ctrl-R / Ctrl-S / Ctrl-C: readline (or
		// the abandoned line) owns the screen here — get the panel out of the way.
		sg.clear()
	default:
		sg.render(string(line))
	}
	return nil, 0, false
}

// render paints the panel for the current line under the input, restoring the
// cursor to where the operator is typing. It saves the cursor (DECSC), drops to
// the next line, clears everything below (wiping any previous panel), writes the
// rows, then restores the cursor (DECRC). When there is nothing to show it just
// clears a panel left over from a previous keystroke.
func (sg *suggester) render(line string) {
	rows := formatSuggestions(suggestMatches(line), sg.style)
	if rows == "" {
		sg.clear()
		return
	}
	var b strings.Builder
	b.WriteString("\0337")                                // DECSC: save cursor
	b.WriteString("\r\n")                                 // move below the input line
	b.WriteString("\033[0J")                              // clear downward (old panel)
	b.WriteString(strings.ReplaceAll(rows, "\n", "\r\n")) // raw-mode line breaks
	b.WriteString("\0338")                                // DECRC: restore cursor
	io.WriteString(sg.out, b.String())
	sg.shown = true
}

// clear removes a painted panel (if any) and leaves the cursor where it was.
func (sg *suggester) clear() {
	if !sg.shown {
		return
	}
	io.WriteString(sg.out, "\0337\r\n\033[0J\0338")
	sg.shown = false
}
