package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
)

// errInterrupted is returned by a lineReader when the operator pressed Ctrl-C on
// a line: the current line is abandoned but the session keeps going (unlike EOF,
// which closes it). The REPL treats it as "discard this line, reprompt".
var errInterrupted = errors.New("interrupted")

// lineReader is the single stdin seam the REPL and the confirm gate both read
// through, so a colourful readline editor and a plain scanner can stand in for
// each other. Keeping one instance shared is what stops the gate's approve read
// from racing the REPL's line read on the same terminal.
type lineReader interface {
	// Prompt shows prompt and returns the next line without its trailing
	// newline. It returns io.EOF when input is exhausted or the operator closed
	// the session (Ctrl-D), and errInterrupted when they cancelled the line
	// (Ctrl-C).
	Prompt(prompt string) (string, error)
	Close() error
}

// scannerReader is the non-interactive lineReader: it reads lines from any
// io.Reader and echoes the prompt to a writer. It backs tests, piped stdin, and
// any non-terminal, so behaviour stays identical to the old bufio.Scanner loop.
type scannerReader struct {
	sc  *bufio.Scanner
	out io.Writer
}

func newScannerReader(r io.Reader, w io.Writer) *scannerReader {
	return &scannerReader{sc: bufio.NewScanner(r), out: w}
}

func (s *scannerReader) Prompt(prompt string) (string, error) {
	if prompt != "" && s.out != nil {
		fmt.Fprint(s.out, prompt)
	}
	if !s.sc.Scan() {
		if err := s.sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return s.sc.Text(), nil
}

func (s *scannerReader) Close() error { return nil }

// readlineReader is the interactive lineReader backed by chzyer/readline: arrow
// keys, in-line editing, persistent history, reverse-search, Tab completion, and
// Ctrl-C / Ctrl-D. readline only holds the terminal in raw mode for the duration
// of one Readline() call and restores cooked mode in between, so every other
// REPL write is an ordinary write to the same terminal.
type readlineReader struct{ rl *readline.Instance }

func (r *readlineReader) Prompt(prompt string) (string, error) {
	r.rl.SetPrompt(prompt)
	line, err := r.rl.Readline()
	switch {
	case errors.Is(err, readline.ErrInterrupt):
		return "", errInterrupted
	case errors.Is(err, io.EOF):
		return "", io.EOF
	}
	return line, err
}

func (r *readlineReader) Close() error { return r.rl.Close() }

// newLineReader returns an interactive readline editor when r is a real terminal
// (with completion sourced from s and history persisted to historyFile), and a
// plain scannerReader otherwise. A terminal that fails to initialise falls back
// to the scanner rather than failing the REPL.
func newLineReader(s *Session, r io.Reader, w io.Writer, historyFile string) lineReader {
	f, ok := r.(*os.File)
	if !ok || !readline.IsTerminal(int(f.Fd())) {
		return newScannerReader(r, w)
	}
	// Stdin is deliberately left to readline's default (os.Stdin): in this branch
	// r is always the real terminal, and the default keeps readline's *os.File
	// raw-mode handling intact (a wrapped reader would hide the fd).
	cfg := &readline.Config{
		Stdout:            w,
		HistoryFile:       historyFile,
		AutoComplete:      newCompleter(s),
		Listener:          &suggester{out: w, style: newStyler(w)},
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	}
	rl, err := readline.NewEx(cfg)
	if err != nil {
		return newScannerReader(r, w)
	}
	return &readlineReader{rl: rl}
}

// newCompleter builds Tab completion for the deterministic `/` commands: every
// command name, and — for the verbs that take an agent id — live agent ids as
// the next word. Unknown input completes to nothing, so free-form natural
// language is never rewritten.
func newCompleter(s *Session) readline.AutoCompleter {
	ids := &agentIDCache{s: s}
	items := make([]readline.PrefixCompleterInterface, 0, len(commandList)+1)
	items = append(items, readline.PcItem("/help"))
	for _, c := range commandList {
		names := append([]string{c.name}, c.aliases...)
		for _, n := range names {
			if c.takesAgentID {
				items = append(items, readline.PcItem(n, readline.PcItemDynamic(ids.complete)))
			} else {
				items = append(items, readline.PcItem(n))
			}
		}
	}
	return readline.NewPrefixCompleter(items...)
}

// agentIDCache serves live agent ids to the Tab completer without hammering the
// daemon: it caches the fleet for a short window and fails open (no suggestions)
// when the daemon is slow or down.
type agentIDCache struct {
	s   *Session
	mu  sync.Mutex
	at  time.Time
	ids []string
}

func (c *agentIDCache) complete(string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 2*time.Second {
		return c.ids
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	sessions, err := c.s.daem.List(ctx)
	if err != nil {
		return c.ids
	}
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	sort.Strings(ids)
	c.ids, c.at = ids, time.Now()
	return ids
}

// historyFilePath is where the interactive editor persists command history
// across sessions. It lives beside warden's other state under ~/.warden.
func historyFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	dir := strings.TrimRight(home, "/") + "/.warden"
	_ = os.MkdirAll(dir, 0o755)
	return dir + "/orch_history"
}
