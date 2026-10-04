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

// tuiLogPath resolves the file setupTUILogging writes to. ok is false when
// logging is disabled (off/discard//dev/null) or no home dir is available.
func tuiLogPath() (path string, ok bool) {
	dest := strings.TrimSpace(os.Getenv("WARDEN_TUI_LOG"))
	if strings.EqualFold(dest, "off") || strings.EqualFold(dest, "discard") || dest == "/dev/null" {
		return "", false
	}
	if dest != "" {
		return dest, true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".warden", "tui.log"), true
}

const logTailMaxBytes = 256 * 1024

// readLogTail returns the last bytes of the TUI log (whole lines) as a string,
// or a human-readable empty-state message when nothing can be shown.
func readLogTail() string {
	path, ok := tuiLogPath()
	if !ok {
		return "Logging is disabled (WARDEN_TUI_LOG=off). Unset it to record TUI logs."
	}
	f, err := os.Open(path)
	if err != nil {
		return "No log file available at " + path + "."
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "Cannot read log file " + path + "."
	}
	start := int64(0)
	if st.Size() > logTailMaxBytes {
		start = st.Size() - logTailMaxBytes
	}
	buf := make([]byte, st.Size()-start)
	n, _ := f.ReadAt(buf, start)
	s := string(buf[:n])
	if start > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return "Log is empty (" + path + ")."
	}
	return s
}

// colorizeLogs tints each line by slog level (ERROR red, WARN yellow, INFO cyan).
func colorizeLogs(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case strings.Contains(l, "level=ERROR") || strings.Contains(l, "ERROR"):
			lines[i] = stError.Render(l)
		case strings.Contains(l, "level=WARN") || strings.Contains(l, "WARN"):
			lines[i] = stAttention.Render(l)
		case strings.Contains(l, "level=INFO") || strings.Contains(l, "INFO"):
			lines[i] = stRunning.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
