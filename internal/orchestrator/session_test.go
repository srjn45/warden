package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func newTestSession(chat llm.Chatter, d Daemon, gate confirmer) *Session {
	return NewSession(chat, d, NewRegistry(), gate, nil)
}

func alwaysReject() *spyGate  { return &spyGate{decision: Decision{Action: Reject}} }
func alwaysApprove() *spyGate { return &spyGate{decision: Decision{Action: Approve}} }

func TestSession_ReadOnlyAutoExecutes(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "list_agents"}}},
		{Text: "2 agents running."},
	}}
	gate := alwaysReject()
	s := newTestSession(chat, &fakeDaemon{sessions: []*store.Session{active("a1"), active("a2")}}, gate)
	out := s.Handle(context.Background(), "what's running?")
	require.Contains(t, out, "2 agents running")
	require.Zero(t, gate.confirmCalls, "reads never hit the gate")
}

func TestSession_MutationRejectedRunsNothing(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development", "prompt": "x"}}}},
		{Text: "ok, didn't spawn."},
	}}
	fd := &fakeDaemon{}
	gate := alwaysReject()
	s := newTestSession(chat, fd, gate)
	s.Handle(context.Background(), "spawn a dev agent")
	require.Equal(t, 1, gate.confirmCalls)
	require.Zero(t, fd.spawnCalls, "reject runs nothing")
}

func TestSession_MutationApprovedRuns(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development", "prompt": "x"}}}},
		{Text: "spawned."},
	}}
	fd := &fakeDaemon{}
	s := newTestSession(chat, fd, alwaysApprove())
	s.Handle(context.Background(), "spawn a dev agent")
	require.Equal(t, 1, fd.spawnCalls)
	require.Equal(t, "development", fd.lastSpawn.Type)
}

func TestSession_UnknownToolRecovers(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "frobnicate"}}},
		{Text: "sorry, can't do that."},
	}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	out := s.Handle(context.Background(), "frobnicate the thing")
	require.Contains(t, out, "can't do that")
	// the unknown-tool error reached the next Chat call's messages
	last := chat.gotMsgs[len(chat.gotMsgs)-1]
	require.Contains(t, lastToolContent(last), "unknown tool")
}

func TestSession_TurnBudgetStops(t *testing.T) {
	// a model that calls a read tool forever
	loop := make([]llm.Reply, maxTurns+2)
	for i := range loop {
		loop[i] = llm.Reply{ToolCalls: []ToolCall{{Name: "list_agents"}}}
	}
	chat := &scriptChatter{replies: loop}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	out := s.Handle(context.Background(), "loop forever")
	require.Contains(t, out, "turn budget")
}

func TestSession_BatchConfirmsOnce(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{
			{Name: "spawn_agent", Args: map[string]any{"prompt": "a"}},
			{Name: "spawn_agent", Args: map[string]any{"prompt": "b"}},
		}},
		{Text: "done."},
	}}
	gate := alwaysApprove()
	fd := &fakeDaemon{}
	s := newTestSession(chat, fd, gate)
	s.Handle(context.Background(), "spawn two agents")
	require.Equal(t, 1, gate.confirmCalls, "a batched plan confirms as one unit")
	require.Equal(t, 2, fd.spawnCalls)
}

func TestSession_DuplicateMutationGatedOnce(t *testing.T) {
	// The model re-proposes the *identical* spawn after it already succeeded — a
	// real small-model failure mode. The operator must be asked exactly once and
	// only one agent must spawn; the loop breaks instead of running to the budget.
	dup := ToolCall{Name: "spawn_agent", Args: map[string]any{"prompt": "x"}}
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{dup}},
		{ToolCalls: []ToolCall{dup}}, // identical re-proposal
		{ToolCalls: []ToolCall{dup}}, // ...and again, in case it kept going
	}}
	fd := &fakeDaemon{}
	gate := alwaysApprove()
	s := newTestSession(chat, fd, gate)
	out := s.Handle(context.Background(), "spawn an agent")
	require.Equal(t, 1, gate.confirmCalls, "the duplicate call is suppressed, not re-gated")
	require.Equal(t, 1, fd.spawnCalls, "only one agent spawns")
	require.Contains(t, out, "spawn_agent", "the operator gets the real outcome, not a generic message")
	require.Less(t, chat.calls, maxTurns, "the loop breaks early instead of spinning to the turn budget")
}

func TestSession_RejectedMutationNotReGated(t *testing.T) {
	// Reject a spawn; the model re-proposes the same call. We must not ask again.
	dup := ToolCall{Name: "spawn_agent", Args: map[string]any{"prompt": "x"}}
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{dup}},
		{ToolCalls: []ToolCall{dup}},
	}}
	fd := &fakeDaemon{}
	gate := alwaysReject()
	s := newTestSession(chat, fd, gate)
	s.Handle(context.Background(), "spawn an agent")
	require.Equal(t, 1, gate.confirmCalls, "a re-proposed rejected call is suppressed, not re-asked")
	require.Zero(t, fd.spawnCalls)
}

func TestSession_DistinctMutationsBothGate(t *testing.T) {
	// Dedup must not collapse genuinely different calls across turns.
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "a"}}}},
		{ToolCalls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "b"}}}},
		{Text: "spawned both."},
	}}
	fd := &fakeDaemon{}
	gate := alwaysApprove()
	s := newTestSession(chat, fd, gate)
	s.Handle(context.Background(), "spawn two different agents")
	require.Equal(t, 2, gate.confirmCalls, "distinct calls each get their own confirm")
	require.Equal(t, 2, fd.spawnCalls)
}

func TestSession_ChatErrorSurfaces(t *testing.T) {
	s := newTestSession(errChatter{err: errors.New("connection refused")}, &fakeDaemon{}, alwaysReject())
	out := s.Handle(context.Background(), "anything")
	require.Contains(t, out, "local model is unavailable")
}

func TestRunREPL_HandlesLinesAndExits(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "hello there"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, nil, strings.NewReader("hi\nexit\n"), &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "hello there")
	require.Equal(t, 1, chat.calls, "exit stops the loop without another turn")
}

func TestRunREPL_SharesScannerWithGate(t *testing.T) {
	// One stdin stream feeds both the REPL line and the gate's approve key.
	chat := &scriptChatter{replies: []llm.Reply{
		{ToolCalls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development", "prompt": "x"}}}},
		{Text: "spawned."},
	}}
	fd := &fakeDaemon{}
	s := NewSession(chat, fd, NewRegistry(), NewGate(strings.NewReader(""), io.Discard), nil)
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, nil, strings.NewReader("spawn a dev agent\na\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, 1, fd.spawnCalls, "the gate read the approve key from the shared REPL scanner")
}

func TestRunREPL_BangRoutesToShellNotModel(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "should not be called"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	var out bytes.Buffer
	sh := &fakeShell{result: RunResult{Captured: "hi\n", ExitCode: 0}, screen: &out}
	err := RunREPL(context.Background(), s, sh, strings.NewReader("!echo hi\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, []string{"echo hi"}, sh.ran, "the !-line ran in the shell")
	require.Zero(t, chat.calls, "a !-line never reaches the model")
	require.Contains(t, out.String(), "hi", "the shell streamed its output verbatim")
}

func TestRunREPL_BareLineRoutesToModel(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "model answered"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	sh := &fakeShell{}
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, sh, strings.NewReader("what's running?\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, 1, chat.calls, "a bare line goes to the model")
	require.Nil(t, sh.ran, "a bare line never touches the shell")
	require.Contains(t, out.String(), "model answered")
}

func TestRunREPL_BangErrorTakesNoAction(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "should not run"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	sh := &fakeShell{result: RunResult{Captured: "boom\n", ExitCode: 1}}
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, sh, strings.NewReader("!make build\nexit\n"), &out)
	require.NoError(t, err)
	require.Zero(t, chat.calls, "on a failing !-command the orchestrator does nothing until asked")
	require.Contains(t, out.String(), "[exit 1]", "the non-zero exit is surfaced")
}

func TestRunREPL_BangOutputEntersContextForNextBareLine(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "looks like a missing import"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	sh := &fakeShell{result: RunResult{Captured: "undefined: Foo\n", ExitCode: 2}}
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, sh, strings.NewReader("!go build\nwhat went wrong?\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, 1, chat.calls, "only the bare follow-up reaches the model")
	// The prior !-command and its output are visible to that follow-up turn.
	msgs := chat.gotMsgs[len(chat.gotMsgs)-1]
	var joined strings.Builder
	for _, m := range msgs {
		joined.WriteString(m.Content)
	}
	require.Contains(t, joined.String(), "go build")
	require.Contains(t, joined.String(), "undefined: Foo")
}

func lastToolContent(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleTool {
			return msgs[i].Content
		}
	}
	return ""
}
