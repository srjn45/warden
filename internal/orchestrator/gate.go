package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Action is the operator's decision on a proposed batch of mutating calls.
type Action int

const (
	// Reject is the safe default: nothing runs. Returned on EOF or an
	// unrecognized key so a stray newline never executes a mutation.
	Reject Action = iota
	Approve
	Edit
)

// Decision is the gate's verdict. For Edit, Calls holds the operator-revised
// calls; for Approve, Calls echoes the approved calls.
type Decision struct {
	Action Action
	Calls  []ToolCall
}

// confirmer is the gate seam the loop depends on, so tests can script it.
type confirmer interface {
	Confirm(calls []ToolCall) Decision
}

// Gate renders proposed mutating calls and reads an approve/edit/reject decision
// from a lineReader seam. Confirming before execution is non-negotiable and not
// config-gated — it is what makes a small local model safe in this seat.
type Gate struct {
	in    lineReader
	out   io.Writer
	style *styler
}

// NewGate builds a gate over the given reader/writer (stdin/stdout in the REPL,
// scripted buffers in tests). By default it reads through a plain scanner; the
// REPL swaps in its shared interactive lineReader via useReader so the gate's
// approve read and the REPL's line read never race the same terminal.
func NewGate(r io.Reader, w io.Writer) *Gate {
	return &Gate{in: newScannerReader(r, w), out: w, style: newStyler(w)}
}

// useReader points the gate at the REPL's shared lineReader (and matching
// styler) so confirmation prompts get the same interactive editor and colours
// as the main loop.
func (g *Gate) useReader(lr lineReader, st *styler) {
	g.in = lr
	if st != nil {
		g.style = st
	}
}

var _ confirmer = (*Gate)(nil)

// Confirm renders every proposed call (a batch confirms as one unit) and blocks
// for [a]pprove / [e]dit / [r]eject. Reject is the default on EOF or an
// unrecognized key.
func (g *Gate) Confirm(calls []ToolCall) Decision {
	fmt.Fprintln(g.out, g.style.heading.Render("orchestrator wants to:"))
	for i, c := range calls {
		fmt.Fprintf(g.out, "  %d. %s %s\n", i+1, g.style.tool.Render(c.Name), renderArgs(c.Args))
	}
	line, err := g.in.Prompt("[a]pprove [e]dit [r]eject: ")
	if err != nil {
		return Decision{Action: Reject} // EOF / interrupt ⇒ safe default
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "approve", "y", "yes":
		return Decision{Action: Approve, Calls: calls}
	case "e", "edit":
		return Decision{Action: Edit, Calls: g.edit(calls)}
	default:
		return Decision{Action: Reject}
	}
}

// edit re-prompts the operator for each call's args as a JSON object. Minimal by
// design for the MVP — a field-by-field editor is a follow-up. An unparseable or
// blank entry keeps the original args for that call.
func (g *Gate) edit(calls []ToolCall) []ToolCall {
	edited := make([]ToolCall, len(calls))
	for i, c := range calls {
		prompt := fmt.Sprintf("edit args for %s (JSON, blank to keep %s): ", c.Name, renderArgs(c.Args))
		nc := ToolCall{Name: c.Name, Args: c.Args}
		if line, err := g.in.Prompt(prompt); err == nil {
			line = strings.TrimSpace(line)
			if line != "" {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err == nil {
					nc.Args = m
				} else {
					fmt.Fprintln(g.out, "  (unparseable JSON — keeping original)")
				}
			}
		}
		edited[i] = nc
	}
	return edited
}

func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}
