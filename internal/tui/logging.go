package tui

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// setupTUILogging isolates logging inside full-screen TUI panes so log
// statements never leak onto stderr/stdout and corrupt the terminal screen.
//
// By default, it writes structured log lines to ~/.warden/tui.log (or the path
// configured in WARDEN_TUI_LOG). If WARDEN_TUI_LOG is "off" or "discard", or
// if opening the log file fails, logging is redirected to io.Discard.
//
// It returns a cleanup function that restores the prior slog default and
// standard library log writer, and closes the log file if opened.
func setupTUILogging() func() {
	prevSlog := slog.Default()
	prevLogWriter := log.Writer()

	w, closer := resolveTUILogWriter()
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
	log.SetOutput(w)

	return func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevLogWriter)
		if closer != nil {
			_ = closer.Close()
		}
	}
}

func resolveTUILogWriter() (io.Writer, io.Closer) {
	dest := strings.TrimSpace(os.Getenv("WARDEN_TUI_LOG"))
	if strings.EqualFold(dest, "off") || strings.EqualFold(dest, "discard") || dest == "/dev/null" {
		return io.Discard, nil
	}
	if dest == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return io.Discard, nil
		}
		wardenDir := filepath.Join(home, ".warden")
		if err := os.MkdirAll(wardenDir, 0o700); err != nil {
			return io.Discard, nil
		}
		dest = filepath.Join(wardenDir, "tui.log")
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return io.Discard, nil
	}
	return f, f
}
