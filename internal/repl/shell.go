package repl

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

// maxCaptureLines caps the shell output the model observes as context. The
// operator's screen is never truncated — only the captured copy that rides into
// the model's context window is tail-bounded, so a chatty command can't blow it.
const maxCaptureLines = 200

// RunResult is one `!` command's outcome. Captured is the tail-bounded copy the
// model observes (never acts on); ExitCode is the shell's $? surfaced verbatim.
type RunResult struct {
	Captured string // tail-bounded, for the model's context
	ExitCode int
}

// ShellRunner is the seam RunREPL routes `!`-lines through, so the REPL can be
// tested against a fake without a real PTY.
type ShellRunner interface {
	Run(ctx context.Context, line string) (RunResult, error)
}

// Shell hosts a persistent child $SHELL on a PTY. It is the operator's own shell
// — started interactive so it sources their rc/profile — so aliases, functions,
// PATH, env, and `--help` output are identical to their normal terminal. warden
// adds capability above the shell and changes nothing about it.
//
// Scope: `!` is NON-INTERACTIVE only — submit a command, stream it to
// completion, return to the prompt. Full-screen/interactive programs (pagers,
// vim, REPLs) belong in the raw-$SHELL escape hatch, not here.
type Shell struct {
	cmd    *exec.Cmd
	ptmx   *os.File
	rd     *bufio.Reader
	screen io.Writer
	marker string // unique command-completion sentinel for this session
	mu     sync.Mutex
}

// NewShell starts $SHELL (defaulting to /bin/sh) in dir, on a PTY, teeing all
// output to screen. dir seeds the shell's cwd, preserving the spawn-dir
// semantics the cockpit's master pane relies on. It primes the session — echo
// off, empty prompt — so captured output is the command's output alone, then
// returns ready to Run.
func NewShell(dir string, screen io.Writer) (*Shell, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-i")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start %s on pty: %w", shell, err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := &Shell{
		cmd:    cmd,
		ptmx:   ptmx,
		rd:     bufio.NewReader(ptmx),
		screen: screen,
		marker: "__warden_eot_" + hex.EncodeToString(b[:]) + "__",
	}
	if err := s.prime(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// prime silences the shell: `stty -echo` so we don't capture our own input, and
// an empty prompt so PS1/PS2 never pollute the stream. Its own (still-echoed)
// output is drained to the bit bucket. A read deadline keeps a missing/odd shell
// from hanging startup — on timeout NewShell fails and callers treat the shell
// as unavailable.
func (s *Shell) prime() error {
	_ = s.ptmx.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer s.ptmx.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(s.ptmx, "stty -echo; PS1=''; PS2=''; PROMPT_COMMAND=''\n"); err != nil {
		return fmt.Errorf("prime shell: %w", err)
	}
	if _, _, err := s.exec("true", io.Discard); err != nil {
		return fmt.Errorf("prime shell: %w", err)
	}
	return nil
}

// Run submits one non-interactive command, streams its output verbatim to the
// screen, and returns a tail-bounded capture plus the exit code. It NEVER acts
// on the output — it reports and returns. Passivity is the invariant: warden
// observes `!` output, it does not interpret, rewrite, or react to it.
func (s *Shell) Run(_ context.Context, line string) (RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	full, code, err := s.exec(line, s.screen)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{Captured: tail(full, maxCaptureLines), ExitCode: code}, nil
}

// exec writes a command followed by a sentinel that echoes $? on its own line,
// then reads until the sentinel — teeing every output line to screen and into
// the (untruncated) capture. Completion is marker-based, not a prompt-regex, so
// "run to completion, return to the prompt" stays deterministic over a
// persistent PTY.
func (s *Shell) exec(line string, screen io.Writer) (string, int, error) {
	// Leading \n guarantees the sentinel lands on its own line even when the
	// command's last output line has no trailing newline.
	cmd := line + "\nprintf '\\n%s %s\\n' '" + s.marker + "' \"$?\"\n"
	if _, err := io.WriteString(s.ptmx, cmd); err != nil {
		return "", 0, fmt.Errorf("write to shell: %w", err)
	}
	var capture strings.Builder
	for {
		raw, err := s.rd.ReadString('\n')
		if err != nil {
			if raw == "" {
				return capture.String(), 0, fmt.Errorf("read shell output: %w", err)
			}
		}
		if trimmed := strings.TrimRight(raw, "\r\n"); strings.HasPrefix(strings.TrimSpace(trimmed), s.marker) {
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), s.marker))
			code, _ := strconv.Atoi(rest)
			return capture.String(), code, nil
		}
		if _, werr := io.WriteString(screen, raw); werr != nil {
			return capture.String(), 0, werr
		}
		capture.WriteString(raw)
	}
}

// Close ends the shell session and releases the PTY.
func (s *Shell) Close() error {
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	return nil
}
