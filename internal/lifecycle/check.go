package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoCheckConfig is returned when a repo has no .warden/check.yml. The caller
// (CLI/MCP) turns it into a friendly "add a config or run your tests directly"
// message — wd check never pretends to know a project's commands.
var ErrNoCheckConfig = errors.New("no .warden/check.yml in this project — add one to register checks, or run your tests directly")

// maxCheckOutputLines caps captured failure output. Test runners and compilers
// print the decisive summary last, so warden keeps the tail and notes the cut —
// the agent reads the failure, not the whole log. (LLM summarization is a later
// phase; this is the deterministic fallback.)
const maxCheckOutputLines = 120

// CheckEntry is one configured check command. It unmarshals from either a bare
// scalar (`test: go test ./...`) or a mapping with an optional working dir
// (`api: {cmd: go test ./..., dir: services/api}`) for monorepo task scoping.
type CheckEntry struct {
	Cmd string `yaml:"cmd"`
	Dir string `yaml:"dir,omitempty"` // sub-dir relative to the repo root; "" = repo root
}

// UnmarshalYAML accepts the scalar shorthand (`name: cmd`) as well as the full
// mapping form, so simple configs stay terse.
func (e *CheckEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&e.Cmd)
	}
	type raw CheckEntry // avoid recursing into this method
	return value.Decode((*raw)(e))
}

// checkConfig is the parsed .warden/check.yml — the single source of truth shared
// by the runner and (Phase 0c-2) the test-redirect hook, so the gate and the
// runner can never drift.
type checkConfig struct {
	Check map[string]CheckEntry `yaml:"check"`
}

// CheckOutcome is one check command's result. Output (captured combined
// stdout+stderr, tail-truncated) is populated ONLY on failure — a passing check
// has nothing the agent needs to read.
type CheckOutcome struct {
	Name     string `json:"name"`
	Cmd      string `json:"cmd"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

// CheckResult aggregates the checks `wd check` / `mcp__warden__check` ran — a
// compact pass/fail summary in place of the hundreds of lines a raw test run
// would spill into the transcript.
type CheckResult struct {
	Passed bool           `json:"passed"` // every check that ran passed
	Checks []CheckOutcome `json:"checks"`
}

// Check runs the project's configured check command(s) in dir and returns a
// pass/fail summary, capturing output only for the checks that failed. name
// selects a single configured entry; "" runs them all (in stable, alphabetical
// order). Commands come from the per-project .warden/check.yml — a repo with no
// config returns ErrNoCheckConfig, and an unknown name returns an error naming
// the configured checks. warden stays language-agnostic: it only runs what the
// project registered.
func (l *Lifecycle) Check(ctx context.Context, dir, name string) (CheckResult, error) {
	cfg, err := loadCheckConfig(dir)
	if err != nil {
		return CheckResult{}, err
	}
	if len(cfg.Check) == 0 {
		return CheckResult{}, ErrNoCheckConfig
	}
	names, err := selectChecks(cfg, name)
	if err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{Passed: true}
	for _, n := range names {
		outcome := l.runCheck(ctx, dir, n, cfg.Check[n])
		if !outcome.Passed {
			res.Passed = false
		}
		res.Checks = append(res.Checks, outcome)
	}
	return res, nil
}

// runCheck executes one entry via `sh -c` (so full command lines, pipes, and the
// project's own runner like `make verify` all work) in dir or its configured
// sub-dir, and records pass/fail plus — on failure only — the truncated output.
func (l *Lifecycle) runCheck(ctx context.Context, dir, name string, entry CheckEntry) CheckOutcome {
	runDir := dir
	if entry.Dir != "" {
		runDir = filepath.Join(dir, entry.Dir)
	}
	out, err := l.run.Run(ctx, runDir, "sh", "-c", entry.Cmd)
	outcome := CheckOutcome{Name: name, Cmd: entry.Cmd, Passed: err == nil}
	if err != nil {
		outcome.ExitCode = exitCode(err)
		outcome.Output = truncateTail(out, maxCheckOutputLines)
	}
	return outcome
}

// CheckCommands returns the project's registered checks as name→command, loaded
// from <dir>/.warden/check.yml. It is the single source the Layer-2 check-redirect
// hook shares with the runner (Check), so the enforcement gate and the runner can
// never drift — both derive from the same parsed config. A missing config yields
// an empty map and no error: the hook then redirects nothing (unknown commands
// pass through), making the feature opt-in per repo by virtue of config existing.
// A malformed config is a hard error the caller can choose to fail open on.
func CheckCommands(dir string) (map[string]string, error) {
	cfg, err := loadCheckConfig(dir)
	if err != nil {
		return nil, err
	}
	cmds := make(map[string]string, len(cfg.Check))
	for name, entry := range cfg.Check {
		cmds[name] = entry.Cmd
	}
	return cmds, nil
}

// loadCheckConfig reads <dir>/.warden/check.yml (or .yaml). A missing file yields
// an empty config (the caller maps that to ErrNoCheckConfig); a present but
// malformed file is a hard error so the operator fixes it rather than silently
// losing checks.
func loadCheckConfig(dir string) (checkConfig, error) {
	b, path, err := readFirst(
		filepath.Join(dir, ".warden", "check.yml"),
		filepath.Join(dir, ".warden", "check.yaml"),
	)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return checkConfig{}, nil
		}
		return checkConfig{}, fmt.Errorf("read check config: %w", err)
	}
	var cfg checkConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return checkConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// readFirst returns the contents of the first path that exists. If none exist it
// returns the fs.ErrNotExist from the last attempt.
func readFirst(paths ...string) (content []byte, path string, err error) {
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e == nil {
			return b, p, nil
		}
		err = e
		if !errors.Is(e, fs.ErrNotExist) {
			return nil, p, e // a real error (perms etc.) — surface it
		}
	}
	return nil, "", err
}

// selectChecks resolves the requested name to the list of check names to run:
// all (alphabetical) when name is "", or just the named one, erroring with the
// configured vocabulary when the name is unknown.
func selectChecks(cfg checkConfig, name string) ([]string, error) {
	if name == "" {
		return sortedKeys(cfg.Check), nil
	}
	if _, ok := cfg.Check[name]; !ok {
		return nil, fmt.Errorf("unknown check %q — configured: %s", name, strings.Join(sortedKeys(cfg.Check), ", "))
	}
	return []string{name}, nil
}

// sortedKeys returns cfg's check names in stable alphabetical order.
func sortedKeys(m map[string]CheckEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// exitCode extracts the process exit code from a Runner error, defaulting to 1
// for a non-ExitError failure (e.g. the command could not be started).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// truncateTail keeps at most maxLines lines of s, retaining the tail (where test
// runners and compilers print the decisive failure) with a leading marker noting
// how many lines were dropped.
func truncateTail(s string, maxLines int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	dropped := len(lines) - maxLines
	tail := lines[dropped:]
	return fmt.Sprintf("… (%d earlier lines truncated)\n%s", dropped, strings.Join(tail, "\n"))
}
