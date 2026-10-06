package agentbackend

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func recorder() (*[]string, func(string) error) {
	var keys []string
	return &keys, func(k string) error { keys = append(keys, k); return nil }
}

func TestAnswerHotkeyMenuSendsDigit(t *testing.T) {
	keys, send := recorder()
	a := &Approval{Options: []string{"Yes", "No"}, SelectedIdx: 1}
	require.NoError(t, Answer(a, 2, send, nil))
	require.Equal(t, []string{"2"}, *keys)
}

func TestAnswerCursorMenuMovesVerifiesThenConfirms(t *testing.T) {
	keys, send := recorder()
	a := &Approval{Options: []string{"No, exit", "Yes, I trust this folder"}, SelectedIdx: 1, Navigate: true}
	reparse := func() (*Approval, bool) {
		return &Approval{Options: a.Options, SelectedIdx: 2, Navigate: true}, true
	}
	require.NoError(t, Answer(a, 2, send, reparse))
	require.Equal(t, []string{"Down", "Enter"}, *keys)

	// Already on the option: no move, still verified, then Enter.
	keys, send = recorder()
	a.SelectedIdx = 2
	require.NoError(t, Answer(a, 2, send, reparse))
	require.Equal(t, []string{"Enter"}, *keys)

	// Moving up.
	keys, send = recorder()
	up := func() (*Approval, bool) { return &Approval{Options: a.Options, SelectedIdx: 1, Navigate: true}, true }
	require.NoError(t, Answer(a, 1, send, up))
	require.Equal(t, []string{"Up", "Enter"}, *keys)
}

// TestAnswerCursorMenuNeverConfirmsWrongOption is the safety property: Enter
// confirms whatever is highlighted, so if the cursor did not land on the target
// (a lost key, a replaced menu) nothing may be confirmed.
func TestAnswerCursorMenuNeverConfirmsWrongOption(t *testing.T) {
	a := &Approval{Options: []string{"No, exit", "Yes, I trust this folder"}, SelectedIdx: 1, Navigate: true}
	for name, reparse := range map[string]func() (*Approval, bool){
		"cursor did not move": func() (*Approval, bool) { return &Approval{Options: a.Options, SelectedIdx: 1}, true },
		"menu replaced":       func() (*Approval, bool) { return &Approval{Options: []string{"Yes", "No"}, SelectedIdx: 2}, true },
		"prompt gone":         func() (*Approval, bool) { return nil, false },
	} {
		t.Run(name, func(t *testing.T) {
			keys, send := recorder()
			err := Answer(a, 2, send, reparse)
			require.True(t, errors.Is(err, ErrPromptChanged))
			require.NotContains(t, *keys, "Enter")
		})
	}

	keys, send := recorder()
	require.Error(t, Answer(&Approval{Options: a.Options, Navigate: true}, 2, send, func() (*Approval, bool) { return nil, false }))
	require.Empty(t, *keys, "unknown cursor position: send nothing")
	require.Error(t, Answer(a, 3, send, nil))
}
