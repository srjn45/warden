// Package logging configures warden's process-wide structured logger.
//
// Setup builds a [slog.Logger] from a level (debug|info|warn|error) and a
// format (text|json) and installs it as the slog default. Because slog's
// default also backs the standard library's log package, every existing
// log.Print call in the tree is routed through the same handler — so the
// chosen level filtering and JSON/text format apply uniformly, even to call
// sites that have not yet been migrated to slog.
//
// Components log through the package-level slog functions (slog.Info, etc.),
// which target the installed default; there is no logger to thread through
// constructors.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// DefaultLevel and DefaultFormat are the fallbacks used when neither a flag nor
// config supplies a value. They mirror the config defaults so behaviour is the
// same whether or not a config file exists.
const (
	DefaultLevel  = "info"
	DefaultFormat = "text"
)

// Levels and Formats enumerate the accepted values (used for validation and to
// build user-facing error/usage text).
var (
	Levels  = []string{"debug", "info", "warn", "error"}
	Formats = []string{"text", "json"}
)

// ParseLevel maps a level name to a [slog.Level]. It is case-insensitive and
// returns an error for an unrecognized name.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "": // empty defaults to info
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want one of %s)", s, strings.Join(Levels, ", "))
	}
}

// ValidLevel reports whether s is an accepted level name (case-insensitive).
func ValidLevel(s string) bool {
	_, err := ParseLevel(s)
	return err == nil
}

// ValidFormat reports whether s is an accepted format name (case-insensitive).
func ValidFormat(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text", "json", "":
		return true
	default:
		return false
	}
}

// Setup builds a logger for the given level and format, installs it as the slog
// default (which also redirects the standard log package), and returns it.
// Invalid level/format values are an error; callers should validate or fall
// back before calling. Output goes to stderr.
func Setup(level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	case "text", "":
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q (want one of %s)", format, strings.Join(Formats, ", "))
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger, nil
}
