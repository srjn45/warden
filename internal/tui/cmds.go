package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
)

type sessionsMsg struct {
	sessions []*store.Session
	err      error
}
type outputMsg struct {
	id   string
	text string
}
type spawnDoneMsg struct {
	id  string
	err error
}
type cleanupDoneMsg struct {
	id       string
	err      error
	conflict bool
}
type inputDoneMsg struct{ err error }
type attachDoneMsg struct{ err error }
type tickMsg time.Time

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
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
			return outputMsg{id: id, text: ""}
		}
		return outputMsg{id: id, text: out}
	}
}

func spawnCmd(a api, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt})
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

func cleanupCmd(a api, id string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		err := a.Cleanup(ctx, id, force, false)
		if err != nil {
			var se *client.StatusError
			if errors.As(err, &se) && se.Code == 409 {
				return cleanupDoneMsg{id: id, conflict: true, err: err}
			}
			return cleanupDoneMsg{id: id, err: err}
		}
		return cleanupDoneMsg{id: id}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}
