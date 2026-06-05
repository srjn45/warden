package tui

import (
	"context"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

type sessionsMsg struct {
	sessions []*store.Session
	err      error
}
type outputMsg struct {
	id   string
	text string
	err  error // non-nil ⇒ fetch failed; keep the last good output, don't blank it
}
type spawnDoneMsg struct {
	id  string
	err error
}
type cleanupDoneMsg struct {
	id  string
	err error
}
type inputDoneMsg struct{ err error }
type attachDoneMsg struct{ err error }
type tickMsg time.Time

type digestMsg struct {
	id     string
	digest *digest.Digest
	err    error
}

func digestCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong() // narrator (claude -p) dominates latency
		defer cancel()
		d, err := a.Digest(ctx, id)
		return digestMsg{id: id, digest: d, err: err}
	}
}

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

func outputCmd(a api, id string) tea.Cmd {
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		out, err := a.Output(ctx, id, 400)
		if err != nil {
			return outputMsg{id: id, err: err}
		}
		return outputMsg{id: id, text: out}
	}
}

func spawnCmd(a api, prompt, cwd string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt, Cwd: cwd})
		if err != nil {
			return spawnDoneMsg{err: err}
		}
		return spawnDoneMsg{id: s.ID}
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

func attachCmd(id string) tea.Cmd {
	c := exec.Command("tmux", "attach", "-t", id)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return attachDoneMsg{err: err}
	})
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

type approvalsMsg struct {
	enabled bool
	views   []approval.View
	err     error
}

type approveResultMsg struct {
	id  string
	err error
}

func approvalsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		on, views, err := a.Approvals(ctx)
		return approvalsMsg{enabled: on, views: views, err: err}
	}
}

func approveCmd(a api, id string, option int, fingerprint string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return approveResultMsg{id: id, err: a.Approve(ctx, id, option, fingerprint)}
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

func retryJobCmd(a api, pid, job string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineRetry(context.Background(), pid, job)}
	}
}
