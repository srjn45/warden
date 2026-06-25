package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeCheckCfg drops a .warden/check.yml with body into a fresh repo dir.
func writeCheckCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".warden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".warden", "check.yml"), []byte(body), 0o644))
	return dir
}

func TestCheckRunsNamedEntry(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n  lint: golangci-lint run\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: "ok", Err: nil},
	}}
	res, err := New(fr, &FakeConfig{}).Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.True(t, res.Passed)
	require.Len(t, res.Checks, 1)
	require.Equal(t, "test", res.Checks[0].Name)
	require.Equal(t, "go test ./...", res.Checks[0].Cmd)
	require.Empty(t, res.Checks[0].Output, "a passing check captures no output")
}

func TestCheckFailureCapturesOutputOnly(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: "--- FAIL: TestX\nFAIL", Err: errStub("exit 1")},
	}}
	res, err := New(fr, &FakeConfig{}).Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.False(t, res.Checks[0].Passed)
	require.Equal(t, 1, res.Checks[0].ExitCode)
	require.Contains(t, res.Checks[0].Output, "--- FAIL: TestX")
}

func TestCheckNoArgRunsAllSortedAggregating(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n  lint: golangci-lint run\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...":     {Out: "ok"},
		"sh -c golangci-lint run": {Out: "issues", Err: errStub("exit 1")},
	}}
	res, err := New(fr, &FakeConfig{}).Check(context.Background(), dir, "")
	require.NoError(t, err)
	require.False(t, res.Passed, "one failing check fails the run")
	require.Len(t, res.Checks, 2)
	// Stable alphabetical order: lint before test.
	require.Equal(t, "lint", res.Checks[0].Name)
	require.Equal(t, "test", res.Checks[1].Name)
}

func TestCheckEntryDirIsScoped(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  api:\n    cmd: go test ./...\n    dir: services/api\n")
	fr := &FakeRunner{}
	_, err := New(fr, &FakeConfig{}).Check(context.Background(), dir, "api")
	require.NoError(t, err)
	// The entry's dir is joined onto the repo root for that command.
	want := filepath.Join(dir, "services/api")
	found := false
	for _, c := range fr.Calls {
		if len(c.Argv) >= 1 && c.Argv[0] == "sh" {
			require.Equal(t, want, c.Dir)
			found = true
		}
	}
	require.True(t, found, "expected an sh -c invocation in the scoped dir")
}

func TestCheckUnknownNameListsConfigured(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n  lint: golangci-lint run\n")
	_, err := New(&FakeRunner{}, &FakeConfig{}).Check(context.Background(), dir, "bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown check")
	require.Contains(t, err.Error(), "lint, test", "the error names the configured checks")
}

func TestCheckNoConfigIsSentinel(t *testing.T) {
	_, err := New(&FakeRunner{}, &FakeConfig{}).Check(context.Background(), t.TempDir(), "")
	require.ErrorIs(t, err, ErrNoCheckConfig)
}

func TestCheckMalformedConfigErrors(t *testing.T) {
	dir := writeCheckCfg(t, "check: [not a map\n")
	_, err := New(&FakeRunner{}, &FakeConfig{}).Check(context.Background(), dir, "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoCheckConfig)
}

func TestCheckScalarAndMappingEntries(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  a: echo hi\n  b:\n    cmd: echo bye\n    dir: sub\n")
	cfg, err := loadCheckConfig(dir)
	require.NoError(t, err)
	require.Equal(t, "echo hi", cfg.Check["a"].Cmd)
	require.Equal(t, "", cfg.Check["a"].Dir)
	require.Equal(t, "echo bye", cfg.Check["b"].Cmd)
	require.Equal(t, "sub", cfg.Check["b"].Dir)
}

func TestTruncateTailKeepsTailWithMarker(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("line\n")
	}
	out := truncateTail(b.String(), 3)
	require.Contains(t, out, "7 earlier lines truncated")
	lines := strings.Split(out, "\n")
	require.Equal(t, 4, len(lines), "marker line + 3 kept tail lines")
	require.Equal(t, []string{"line", "line", "line"}, lines[1:])
}

func TestTruncateTailShortIsUnchanged(t *testing.T) {
	require.Equal(t, "a\nb", truncateTail("a\nb\n", 10))
	require.Equal(t, "", truncateTail("\n\n", 10))
}

// bigLog builds a failure log of n lines, well past maxCheckOutputLines.
func bigLog(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("noise noise noise\n")
	}
	b.WriteString("--- FAIL: TestZ\nFAIL")
	return b.String()
}

func TestCheckSummarizesOversizedFailureWithLLM(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: bigLog(maxCheckOutputLines + 50), Err: errStub("exit 1")},
	}}
	fc := &fakeCompleter{out: "TestZ: assertion failed\n"}
	lc := New(fr, &FakeConfig{})
	lc.LLM = fc

	res, err := lc.Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.False(t, res.Passed)
	require.Equal(t, 1, fc.calls, "an oversized failure is condensed by the local model")
	require.Contains(t, res.Checks[0].Output, checkSummaryMarker)
	require.Contains(t, res.Checks[0].Output, "TestZ: assertion failed")
}

func TestCheckDoesNotSummarizeSmallFailure(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: "--- FAIL: TestX\nFAIL", Err: errStub("exit 1")},
	}}
	fc := &fakeCompleter{out: "should not be used"}
	lc := New(fr, &FakeConfig{})
	lc.LLM = fc

	res, err := lc.Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.Equal(t, 0, fc.calls, "a within-cap failure needs no model — the raw tail is small enough")
	require.NotContains(t, res.Checks[0].Output, checkSummaryMarker)
	require.Contains(t, res.Checks[0].Output, "--- FAIL: TestX")
}

func TestCheckFallsBackToTailWhenLLMErrors(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: bigLog(maxCheckOutputLines + 50), Err: errStub("exit 1")},
	}}
	fc := &fakeCompleter{err: errStub("connection refused")}
	lc := New(fr, &FakeConfig{})
	lc.LLM = fc

	res, err := lc.Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.Equal(t, 1, fc.calls)
	require.NotContains(t, res.Checks[0].Output, checkSummaryMarker, "a model error degrades to the deterministic tail")
	require.Contains(t, res.Checks[0].Output, "earlier lines truncated")
	require.Contains(t, res.Checks[0].Output, "--- FAIL: TestZ")
}

func TestCheckFallsBackToTailWhenLLMEmpty(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: bigLog(maxCheckOutputLines + 50), Err: errStub("exit 1")},
	}}
	fc := &fakeCompleter{out: "   \n"} // empty reply → keep the tail
	lc := New(fr, &FakeConfig{})
	lc.LLM = fc

	res, err := lc.Check(context.Background(), dir, "test")
	require.NoError(t, err)
	require.Equal(t, 1, fc.calls)
	require.NotContains(t, res.Checks[0].Output, checkSummaryMarker)
	require.Contains(t, res.Checks[0].Output, "--- FAIL: TestZ")
}

func TestCheckNoLLMKeepsDeterministicTail(t *testing.T) {
	dir := writeCheckCfg(t, "check:\n  test: go test ./...\n")
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"sh -c go test ./...": {Out: bigLog(maxCheckOutputLines + 50), Err: errStub("exit 1")},
	}}
	res, err := New(fr, &FakeConfig{}).Check(context.Background(), dir, "test") // LLM nil
	require.NoError(t, err)
	require.NotContains(t, res.Checks[0].Output, checkSummaryMarker)
	require.Contains(t, res.Checks[0].Output, "earlier lines truncated")
}

func TestOversizedOutput(t *testing.T) {
	require.False(t, oversizedOutput(""))
	require.False(t, oversizedOutput("one line"))
	require.False(t, oversizedOutput(strings.Repeat("x\n", maxCheckOutputLines)), "exactly at the cap is not oversized")
	require.True(t, oversizedOutput(strings.Repeat("x\n", maxCheckOutputLines+1)))
}

func TestParseCheckSummaryCapsLines(t *testing.T) {
	require.Equal(t, "", parseCheckSummary("   \n  "))
	require.Equal(t, "a\nb", parseCheckSummary("  a\nb\n"))
	capped := parseCheckSummary(strings.Repeat("f\n", maxCheckOutputLines+20))
	require.Equal(t, maxCheckOutputLines, len(strings.Split(capped, "\n")), "a runaway model reply is capped to the line budget")
}
