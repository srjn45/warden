package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// registeredChecks mirrors a typical .warden/check.yml for the matcher tests.
func registeredChecks() map[string]string {
	return map[string]string{
		"test":  "go test ./...",
		"lint":  "golangci-lint run",
		"build": "go build ./...",
		"all":   "make verify",
		"api":   "go test ./...", // monorepo entry, different name/dir, same cmd
	}
}

func TestDetectCheckRedirectMatches(t *testing.T) {
	checks := map[string]string{
		"test":  "go test ./...",
		"lint":  "golangci-lint run",
		"build": "go build ./...",
		"all":   "make verify",
	}
	cases := []struct {
		name, cmd, wantCheck string
	}{
		{"exact", "go test ./...", "test"},
		{"extra trailing flag", "go test ./... -count=1", "test"},
		{"env prefix", "CGO_ENABLED=0 go test ./...", "test"},
		{"after cd", "cd web && go test ./...", "test"},
		{"make target", "make verify", "all"},
		{"make extra flag", "make verify --jobs 4", "all"},
		{"lint", "golangci-lint run", "lint"},
		{"piped", "echo hi | go build ./...", "build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, cmd := detectCheckRedirect(tc.cmd, checks)
			require.Equal(t, tc.wantCheck, name, "for %q", tc.cmd)
			require.Equal(t, checks[tc.wantCheck], cmd)
		})
	}
}

func TestDetectCheckRedirectPassesThrough(t *testing.T) {
	checks := map[string]string{
		"test":  "go test ./...",
		"build": "go build ./...",
	}
	allowed := []string{
		"go test -run TestX ./internal", // focused subset — narrower than the registered command
		"go test",                       // bare, shorter than the signature
		"go build",                      // ditto
		"pytest",                        // not a registered command
		"npm test",                      // wrong stack — not registered here
		"ls -la",                        // not a check at all
		`echo "go test ./..."`,          // the command only appears in a quoted string
		"go vet ./...",                  // a real go subcommand, but not registered
		"",                              // empty
		"goimports -w go test ./...",    // not led by the signature
	}
	for _, cmd := range allowed {
		name, _ := detectCheckRedirect(cmd, checks)
		require.Empty(t, name, "should pass through %q", cmd)
	}
}

func TestDetectCheckRedirectNoConfigRedirectsNothing(t *testing.T) {
	name, _ := detectCheckRedirect("go test ./...", map[string]string{})
	require.Empty(t, name, "with no registered checks nothing is redirected")
}

// When two checks could match the same command, the alphabetically-first name is
// chosen so the redirect message is deterministic.
func TestDetectCheckRedirectIsDeterministic(t *testing.T) {
	checks := map[string]string{"test": "go test ./...", "api": "go test ./..."}
	name, _ := detectCheckRedirect("go test ./...", checks)
	require.Equal(t, "api", name)
}

func TestCheckSignatureStripsEnvAssignments(t *testing.T) {
	require.Equal(t, []string{"go", "test", "./..."}, checkSignature("FOO=1 BAR=2 go test ./..."))
	require.Equal(t, []string{"make", "verify"}, checkSignature("make verify"))
	require.Nil(t, checkSignature(""))
}

// runCheckGuardHook drives the `hook check-guard` command with the given stdin JSON.
func runCheckGuardHook(t *testing.T, stdin string) string {
	t.Helper()
	cmd := newHookCheckGuardCmd()
	cmd.SetIn(strings.NewReader(stdin))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.NoError(t, cmd.Execute())
	return out.String()
}

// writeCheckCfg writes a .warden/check.yml under dir and returns dir.
func writeCheckCfg(t *testing.T, dir, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "check.yml"), []byte(body), 0o600))
	return dir
}

func bashHookInput(t *testing.T, command, cwd string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": command},
		"cwd":        cwd,
	})
	require.NoError(t, err)
	return string(b)
}

func TestHookCheckGuardDeniesRegisteredCommand(t *testing.T) {
	dir := writeCheckCfg(t, t.TempDir(), "check:\n  test: go test ./...\n  lint: go vet ./...\n")
	out := runCheckGuardHook(t, bashHookInput(t, "go test ./...", dir))

	var dec preToolUseDecision
	require.NoError(t, json.Unmarshal([]byte(out), &dec))
	require.Equal(t, "deny", dec.HookSpecificOutput.PermissionDecision)
	require.Equal(t, "PreToolUse", dec.HookSpecificOutput.HookEventName)
	reason := dec.HookSpecificOutput.PermissionDecisionReason
	require.Contains(t, reason, "mcp__warden__check")
	require.Contains(t, reason, "wd check test", "names the matched check so Claude can re-issue it")
}

func TestHookCheckGuardAllowsUnregisteredCommand(t *testing.T) {
	dir := writeCheckCfg(t, t.TempDir(), "check:\n  test: go test ./...\n")
	// A focused subset the registered command can't reproduce → not redirected.
	require.Empty(t, runCheckGuardHook(t, bashHookInput(t, "go test -run TestFoo ./internal/cli", dir)))
}

func TestHookCheckGuardNoConfigAllows(t *testing.T) {
	// A repo with no .warden/check.yml redirects nothing — the feature is opt-in.
	require.Empty(t, runCheckGuardHook(t, bashHookInput(t, "go test ./...", t.TempDir())))
}

func TestHookCheckGuardIgnoresNonBash(t *testing.T) {
	dir := writeCheckCfg(t, t.TempDir(), "check:\n  test: go test ./...\n")
	in := `{"tool_name":"Edit","tool_input":{"file_path":"/x"},"cwd":"` + dir + `"}`
	require.Empty(t, runCheckGuardHook(t, in))
}

func TestHookCheckGuardFailsOpenOnGarbage(t *testing.T) {
	require.Empty(t, runCheckGuardHook(t, "not json"))
}

func TestHookCheckGuardFailsOpenOnMalformedConfig(t *testing.T) {
	dir := writeCheckCfg(t, t.TempDir(), "check: [this is not a map\n")
	// A broken config must never wedge the agent — the hook allows the command.
	require.Empty(t, runCheckGuardHook(t, bashHookInput(t, "go test ./...", dir)))
}

func TestHookCheckGuardScopedEntryRedirects(t *testing.T) {
	// A monorepo entry registered via the mapping form (cmd:/dir:) still redirects.
	dir := writeCheckCfg(t, t.TempDir(), "check:\n  api:\n    cmd: go test ./...\n    dir: services/api\n")
	out := runCheckGuardHook(t, bashHookInput(t, "go test ./...", dir))
	var dec preToolUseDecision
	require.NoError(t, json.Unmarshal([]byte(out), &dec))
	require.Equal(t, "deny", dec.HookSpecificOutput.PermissionDecision)
	require.Contains(t, dec.HookSpecificOutput.PermissionDecisionReason, "wd check api")
}

func TestRegisteredChecksFixtureMatches(t *testing.T) {
	// Guard against the fixture drifting away from a realistic config shape.
	name, _ := detectCheckRedirect("make verify", registeredChecks())
	require.Equal(t, "all", name)
}
