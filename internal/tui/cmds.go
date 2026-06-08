package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

type sessionsMsg struct {
	sessions []*store.Session
	err      error
}
type spawnDoneMsg struct {
	id      string
	err     error
	confirm *client.ErrConfirmationRequired // non-nil ⇒ daemon's memory-pressure gate warned (HTTP 428)
}
type cleanupDoneMsg struct {
	id  string
	err error
}
type inputDoneMsg struct{ err error }
type attachDoneMsg struct{ err error }
type tickMsg time.Time

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// bgLong is for synchronous daemon operations that legitimately take longer than
// a read — spawn runs git worktree add + tmux setup on the daemon. A short
// deadline here would abort a slow-but-successful spawn (the daemon keeps going),
// leaving an orphaned session the TUI thinks failed.
func bgLong() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func listCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ss, err := a.List(ctx)
		return sessionsMsg{sessions: ss, err: err}
	}
}

func spawnCmd(a api, prompt, cwd string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt, Cwd: cwd, Force: force})
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return spawnDoneMsg{confirm: cre}
			}
			return spawnDoneMsg{err: err}
		}
		return spawnDoneMsg{id: s.ID}
	}
}

type pressureMsg struct {
	status client.PressureStatus
	err    error
}

func pressureCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ps, err := a.Pressure(ctx)
		return pressureMsg{status: ps, err: err}
	}
}

func inputCmd(a api, id, text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return inputDoneMsg{err: a.Input(ctx, id, text)}
	}
}

// killCmd kills the agent and removes it from the list in one action: it
// terminates the tmux+claude session, then soft-deletes (archives) the record
// so it drops off the list while staying recoverable in closed/. Mirrors
// `agentctl done`. Terminate is best-effort — an already-dead agent (status
// done/orphaned) errors there, which we ignore so its lingering record can
// still be removed; only a delete failure is surfaced.
func killCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		_ = a.Terminate(ctx, id)
		return cleanupDoneMsg{id: id, err: a.Delete(ctx, id, false)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// dirListMsg carries a /fs/dirs listing back for tab-completion. typed is the
// expanded path the user had typed when completion was requested.
type dirListMsg struct {
	typed   string
	listing client.DirListing
	err     error
}

// openDirMsg is the result of validating a dir the user asked to open.
type openDirMsg struct {
	dir string
	err error
}

// listDirsCmd fetches listDir's subdirectories for completing `typed`.
func listDirsCmd(a api, typed, listDir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		l, err := a.ListDirs(ctx, listDir)
		return dirListMsg{typed: typed, listing: l, err: err}
	}
}

// openDirCmd validates that dir is a readable directory (via /fs/dirs).
func openDirCmd(a api, dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		_, err := a.ListDirs(ctx, dir)
		return openDirMsg{dir: dir, err: err}
	}
}

// contextMsg / messagesMsg carry the inspector's read-only fetches. On error we
// keep the last good data (like outputMsg) so a transient blip doesn't blank the
// view the user is reading.
type contextMsg struct {
	entries []client.ContextEntry
	err     error
}
type messagesMsg struct {
	messages []client.Message
	err      error
}

func contextCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		es, err := a.CtxList(ctx, "")
		return contextMsg{entries: es, err: err}
	}
}

// inspectorMsgLimit bounds the recent-message fetch behind the inspector — a
// full pane's worth without unbounded reads.
const inspectorMsgLimit = 100

func messagesCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ms, err := a.MsgRecent(ctx, inspectorMsgLimit)
		return messagesMsg{messages: ms, err: err}
	}
}

type pipelinesMsg struct {
	pipelines []*pipeline.Pipeline
	err       error
}
type pipelineActionMsg struct{ err error }

func pipelinesCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ps, err := a.PipelineList(context.Background())
		return pipelinesMsg{pipelines: ps, err: err}
	}
}

func cancelPipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineCancel(context.Background(), pid)}
	}
}

func deletePipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineDelete(context.Background(), pid)}
	}
}

func retryJobCmd(a api, pid, job string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineRetry(context.Background(), pid, job)}
	}
}
