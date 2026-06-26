package orchestrator

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// scannerReader echoes the prompt and returns lines, then io.EOF at the end.
func TestScannerReader_PromptsAndReadsThenEOF(t *testing.T) {
	var out bytes.Buffer
	lr := newScannerReader(strings.NewReader("first\nsecond\n"), &out)
	l1, err := lr.Prompt("> ")
	require.NoError(t, err)
	require.Equal(t, "first", l1)
	l2, err := lr.Prompt("> ")
	require.NoError(t, err)
	require.Equal(t, "second", l2)
	_, err = lr.Prompt("> ")
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "> > > ", out.String(), "every prompt is echoed to the writer")
}

// A non-terminal reader yields the scanner backend (no panic, no readline).
func TestNewLineReader_NonTTYFallsBackToScanner(t *testing.T) {
	s := newTestSession(&scriptChatter{}, &fakeDaemon{}, alwaysReject())
	lr := newLineReader(s, strings.NewReader("hi\n"), io.Discard, "")
	_, ok := lr.(*scannerReader)
	require.True(t, ok, "a piped reader must not enter raw-mode readline")
}

// In the REPL, a `/` line is handled deterministically and never reaches the model.
func TestRunREPL_SlashRoutesToCommandsNotModel(t *testing.T) {
	chat := &scriptChatter{}
	fd := &fakeDaemon{sessions: []*store.Session{active("a1")}}
	s := newTestSession(chat, fd, alwaysReject())
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, nil, strings.NewReader("/agents\nexit\n"), &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "a1")
	require.Zero(t, chat.calls, "a /command line never reaches the model")
}

// A mutating `/` line in the REPL still funnels through the gate.
func TestRunREPL_SlashMutationConfirmsViaGate(t *testing.T) {
	fd := &fakeDaemon{}
	s := NewSession(&scriptChatter{}, fd, NewRegistry(), NewGate(strings.NewReader(""), io.Discard), nil)
	var out bytes.Buffer
	// The approve key rides the same stdin stream the REPL reads, since the gate
	// shares the REPL's reader.
	err := RunREPL(context.Background(), s, nil, strings.NewReader("/spawn do the thing\na\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, 1, fd.spawnCalls, "the gate approved the /spawn from the shared reader")
}

// The banner and hint render (plain, since the writer is not a terminal).
func TestRunREPL_ShowsBannerAndHint(t *testing.T) {
	s := newTestSession(&scriptChatter{replies: []llm.Reply{}}, &fakeDaemon{}, alwaysReject())
	var out bytes.Buffer
	require.NoError(t, RunREPL(context.Background(), s, nil, strings.NewReader("exit\n"), &out))
	require.Contains(t, out.String(), "warden interactive")
	require.Contains(t, out.String(), "type / for commands")
}
