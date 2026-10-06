package agentbackend

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrPromptChanged is returned by Answer when the prompt moved underneath it
// (the cursor did not land on the requested option, or the menu was replaced),
// so nothing was confirmed.
var ErrPromptChanged = errors.New("prompt changed before it could be answered")

// Answer selects option idx (1-based) of approval a by sending keystrokes
// through send, one key per call (tmux key names: "1", "Down", "Enter", …).
//
// A hotkey menu (Navigate=false) takes the option's number, which selects and
// confirms in one keystroke. A cursor menu (Navigate=true) is answered by moving
// the cursor from SelectedIdx onto idx and pressing Enter. Because Enter confirms
// whatever is highlighted — and the highlighted default can be a "No, exit" —
// the move is verified first: reparse re-captures and re-parses the live pane,
// and Enter is sent only when the same menu shows the cursor on idx. reparse may
// be nil only for hotkey menus.
func Answer(a *Approval, idx int, send func(key string) error, reparse func() (*Approval, bool)) error {
	if a == nil || idx < 1 || idx > len(a.Options) {
		return fmt.Errorf("option %d out of range", idx)
	}
	if !a.Navigate {
		return send(strconv.Itoa(idx))
	}
	if a.SelectedIdx < 1 {
		return errors.New("cursor position unknown; cannot navigate to the option")
	}
	if reparse == nil {
		return errors.New("a cursor menu needs a pane re-capture to verify the selection")
	}
	key, steps := "Down", idx-a.SelectedIdx
	if steps < 0 {
		key, steps = "Up", -steps
	}
	for i := 0; i < steps; i++ {
		if err := send(key); err != nil {
			return err
		}
	}
	now, ok := reparse()
	if !ok || now == nil || now.SelectedIdx != idx || !sameOptions(a.Options, now.Options) {
		return ErrPromptChanged
	}
	return send("Enter")
}

func sameOptions(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
