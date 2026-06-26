package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// A read-only `/` command auto-executes without the model and without the gate.
func TestRunCommand_ReadAutoExecutes(t *testing.T) {
	chat := &scriptChatter{}
	gate := alwaysReject()
	s := newTestSession(chat, &fakeDaemon{sessions: []*store.Session{active("a1"), active("a2")}}, gate)
	out, handled := s.RunCommand(context.Background(), "/agents")
	require.True(t, handled)
	require.Contains(t, out, "a1")
	require.Zero(t, chat.calls, "a /command never reaches the model")
	require.Zero(t, gate.confirmCalls, "reads never hit the gate")
}

// A mutating `/` command still passes through the confirm gate before running.
func TestRunCommand_MutationGoesThroughGate(t *testing.T) {
	chat := &scriptChatter{}
	gate := alwaysApprove()
	fd := &fakeDaemon{}
	s := newTestSession(chat, fd, gate)
	_, handled := s.RunCommand(context.Background(), "/spawn refactor the auth package")
	require.True(t, handled)
	require.Equal(t, 1, gate.confirmCalls, "the mutation was confirmed")
	require.Equal(t, 1, fd.spawnCalls)
	require.Equal(t, "refactor the auth package", fd.lastSpawn.Prompt, "the whole tail becomes the prompt")
	require.Zero(t, chat.calls, "no model involved")
}

// A rejected mutating `/` command runs nothing.
func TestRunCommand_MutationRejectedRunsNothing(t *testing.T) {
	fd := &fakeDaemon{}
	s := newTestSession(&scriptChatter{}, fd, alwaysReject())
	s.RunCommand(context.Background(), "/stop a1")
	require.Zero(t, fd.terminateCalls)
}

// Args map onto the right tool fields.
func TestRunCommand_ArgsMapToToolFields(t *testing.T) {
	fd := &fakeDaemon{}
	s := newTestSession(&scriptChatter{}, fd, alwaysApprove())
	s.RunCommand(context.Background(), "/tell a1 run the tests please")
	require.Equal(t, 1, fd.inputCalls)
}

// Unknown `/verb` is handled with a hint — it never falls through to the model.
func TestRunCommand_UnknownIsHandledNotModeled(t *testing.T) {
	chat := &scriptChatter{}
	s := newTestSession(chat, &fakeDaemon{}, alwaysReject())
	out, handled := s.RunCommand(context.Background(), "/nope")
	require.True(t, handled)
	require.Contains(t, out, "unknown command")
	require.Zero(t, chat.calls)
}

// A missing required arg yields the usage line, runs nothing.
func TestRunCommand_MissingArgShowsUsage(t *testing.T) {
	fd := &fakeDaemon{}
	s := newTestSession(&scriptChatter{}, fd, alwaysApprove())
	out, handled := s.RunCommand(context.Background(), "/agent")
	require.True(t, handled)
	require.Contains(t, out, "usage:")
}

// A non-slash line is not a command — the caller routes it to the model.
func TestRunCommand_BareLineNotHandled(t *testing.T) {
	s := newTestSession(&scriptChatter{}, &fakeDaemon{}, alwaysReject())
	_, handled := s.RunCommand(context.Background(), "what is running?")
	require.False(t, handled)
}

// /help lists the commands and the always-available specials.
func TestCommandHelp_ListsCommands(t *testing.T) {
	h := commandHelp()
	require.Contains(t, h, "/spawn")
	require.Contains(t, h, "/agents")
	require.Contains(t, h, "!<cmd>")
}

// Every command's name and aliases are unique across the table.
func TestCommandIndex_NoCollisions(t *testing.T) {
	seen := map[string]string{}
	for _, c := range commandList {
		for _, n := range append([]string{c.name}, c.aliases...) {
			require.True(t, strings.HasPrefix(n, "/"), "%s must start with /", n)
			if prev, dup := seen[n]; dup {
				t.Fatalf("name %q used by both %q and %q", n, prev, c.name)
			}
			seen[n] = c.name
		}
	}
}
