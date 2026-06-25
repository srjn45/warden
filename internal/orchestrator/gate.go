package orchestrator

import (
	"bufio"
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
// from an io seam. Confirming before execution is non-negotiable and not
// config-gated — it is what makes a small local model safe in this seat.
type Gate struct {
	in  *bufio.Scanner
	out io.Writer
}

// NewGate builds a gate over the given reader/writer (stdin/stdout in the REPL,
// scripted buffers in tests).
func NewGate(r io.Reader, w io.Writer) *Gate {
	return &Gate{in: bufio.NewScanner(r), out: w}
}

var _ confirmer = (*Gate)(nil)

// Confirm renders every proposed call (a batch confirms as one unit) and blocks
// for [a]pprove / [e]dit / [r]eject. Reject is the default on EOF or an
// unrecognized key.
func (g *Gate) Confirm(calls []ToolCall) Decision {
	fmt.Fprintln(g.out, "orchestrator wants to:")
	for i, c := range calls {
		fmt.Fprintf(g.out, "  %d. %s %s\n", i+1, c.Name, renderArgs(c.Args))
	}
	fmt.Fprint(g.out, "[a]pprove [e]dit [r]eject: ")

	if !g.in.Scan() {
		return Decision{Action: Reject} // EOF ⇒ safe default
	}
	switch strings.ToLower(strings.TrimSpace(g.in.Text())) {
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
		fmt.Fprintf(g.out, "edit args for %s (JSON, blank to keep %s): ", c.Name, renderArgs(c.Args))
		nc := ToolCall{Name: c.Name, Args: c.Args}
		if g.in.Scan() {
			line := strings.TrimSpace(g.in.Text())
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
