package lifecycle

import (
	"context"
	"os/exec"
	"strings"
)

// Runner executes an external command in an optional working directory and
// returns combined stdout. It is the single seam mocked in tests.
type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- test double ---

type FakeResp struct {
	Out string
	Err error
}

type FakeCall struct {
	Dir  string
	Argv []string
}

// FakeRunner matches on "name arg1 arg2 ..." joined by spaces.
type FakeRunner struct {
	Responses map[string]FakeResp
	Calls     []FakeCall
	// FailIf, when set and it returns a non-nil error for a call's argv, fails
	// that call. Use it to inject a failure on a command whose exact args aren't
	// known in advance (e.g. send-keys, which embeds a random claude session id).
	FailIf func(argv []string) error
}

func (f *FakeRunner) Run(_ context.Context, dir, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.Calls = append(f.Calls, FakeCall{Dir: dir, Argv: argv})
	if f.FailIf != nil {
		if err := f.FailIf(argv); err != nil {
			return "", err
		}
	}
	key := strings.Join(argv, " ")
	if r, ok := f.Responses[key]; ok {
		return r.Out, r.Err
	}
	return "", nil // unmatched calls succeed silently
}
