package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/store"
)

// attachEnv forces TERM=xterm-256color for the tmux attach PTY. The rendering
// endpoint is always xterm.js, and the daemon (started by launchd) usually has
// no TERM at all — without a usable terminfo entry tmux refuses to attach with
// "open terminal failed: terminal does not support clear". Any inherited TERM is
// replaced so tmux emits xterm-256color sequences regardless of how the daemon
// was launched.
func attachEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "TERM=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "TERM=xterm-256color")
}

// resizeMsg is the JSON body of a client text control frame.
type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// parseResize decodes a client text frame into terminal dimensions. ok is false
// for malformed JSON or non-positive dimensions.
func parseResize(data []byte) (cols, rows uint16, ok bool) {
	var m resizeMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, 0, false
	}
	if m.Cols == 0 || m.Rows == 0 {
		return 0, 0, false
	}
	return m.Cols, m.Rows, true
}

// handleAttach bridges an interactive `tmux attach` to the browser over a
// WebSocket. A PTY runs the attach; its output streams to the client as binary
// frames; client binary frames are keystrokes written to the PTY; client text
// frames are {cols,rows} resize controls. Closing the socket detaches THIS
// client only — the agent's tmux session keeps running.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The most recently active client drives the window size (web vs TUI vs
	// terminal). Best-effort; failure is non-fatal.
	_ = exec.Command("tmux", "set-option", "-t", sess.TmuxSession, "window-size", "latest").Run()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the HTTP error response
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20) // 1 MiB — tolerate large pastes

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel() // safety net for early returns (e.g. pty.Start failure)

	cmd := exec.CommandContext(ctx, "tmux", "attach-session", "-t", sess.TmuxSession)
	cmd.Env = attachEnv(os.Environ())
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "pty start failed")
		return
	}
	// Teardown (runs before the earlier `defer cancel()`): close the PTY to
	// unblock the reader goroutine, cancel the context so CommandContext kills the
	// tmux attach, then reap the process so it doesn't linger as a zombie for the
	// daemon's lifetime.
	defer func() {
		_ = ptyFile.Close()
		cancel()
		_ = cmd.Wait()
	}()

	// PTY → client.
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptyFile.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// client → PTY: binary = keystrokes, text = resize control.
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return
		}
		switch typ {
		case websocket.MessageText:
			if cols, rows, ok := parseResize(data); ok {
				_ = pty.Setsize(ptyFile, &pty.Winsize{Cols: cols, Rows: rows})
			}
		case websocket.MessageBinary:
			if _, werr := ptyFile.Write(data); werr != nil {
				return
			}
		}
	}
}
