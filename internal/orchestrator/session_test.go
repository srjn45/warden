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

func TestSession_ChatErrorSurfaces(t *testing.T) {
	s := newTestSession(errChatter{err: errors.New("connection refused")}, &fakeDaemon{}, alwaysReject())
	out := s.Handle(context.Background(), "anything")
	require.Contains(t, out, "local model is unavailable")
}

func TestRunREPL_HandlesLinesAndExits(t *testing.T) {
	chat := &scriptChatter{replies: []llm.Reply{{Text: "hello there"}}}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	var out bytes.Buffer
	err := RunREPL(context.Background(), s, strings.NewReader("hi\nexit\n"), &out)
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
	err := RunREPL(context.Background(), s, strings.NewReader("spawn a dev agent\na\nexit\n"), &out)
	require.NoError(t, err)
	require.Equal(t, 1, fd.spawnCalls, "the gate read the approve key from the shared REPL scanner")
}

func lastToolContent(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleTool {
			return msgs[i].Content
		}
	}
	return ""
}
