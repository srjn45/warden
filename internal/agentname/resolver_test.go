package agentname

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

// stubRunner is a BackendRunner test double. When delay > 0 it blocks until the
// context is cancelled (or delay elapses), simulating a slow subscription CLI.
type stubRunner struct {
	out   string
	err   error
	delay time.Duration
	got   string // last prompt passed to Run
}

func (s *stubRunner) Run(ctx context.Context, prompt string) (string, error) {
	s.got = prompt
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.delay):
		}
	}
	return s.out, s.err
}

func TestResolvePromptNameValidKebab(t *testing.T) {
	r := &stubRunner{out: "ws-leak-fix"}
	name, err := ResolvePromptName(context.Background(), "Fix memory leak in websocket listener", r)
	require.NoError(t, err)
	require.Equal(t, "ws-leak-fix", name)
	require.NoError(t, store.ValidateName(name))
	require.Contains(t, r.got, nameResolveInstruction)
	require.Contains(t, r.got, "Fix memory leak")
}

func TestResolvePromptNameStripsMarkdownAndPrefixes(t *testing.T) {
	cases := map[string]string{
		"`auth-refactor`":                  "auth-refactor",
		"\"billing-module-fix\"":           "billing-module-fix",
		"```\norder-api-builder\n```":      "order-api-builder",
		"```text\nquiet-stream\n```":       "quiet-stream",
		"Sure, here is: `swift-falcon`":    "swift-falcon",
		"Here is the slug: pane-crash-fix": "pane-crash-fix",
		"The name is ws-leak-fix":          "ws-leak-fix",
		"Name: memory-leak-patch":          "memory-leak-patch",
		"  AUTH_REFACTOR  ":                "auth-refactor",
		"auth refactor helper":             "auth-refactor-helper",
	}
	for in, want := range cases {
		r := &stubRunner{out: in}
		name, err := ResolvePromptName(context.Background(), "task", r)
		require.NoError(t, err, in)
		require.Equal(t, want, name, in)
		require.NoError(t, store.ValidateName(name), in)
	}
}

func TestResolvePromptNameTimeoutFallsBack(t *testing.T) {
	r := &stubRunner{out: "should-not-win", delay: 5 * time.Second}
	start := time.Now()
	name, err := ResolvePromptName(context.Background(), "slow task", r)
	elapsed := time.Since(start)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 3*time.Second, "must not wait for the slow CLI")
	requireCodename(t, name)
}

func TestResolvePromptNameCLIErrorFallsBack(t *testing.T) {
	r := &stubRunner{err: errors.New("network down")}
	name, err := ResolvePromptName(context.Background(), "task", r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "network down")
	requireCodename(t, name)
}

func TestResolvePromptNameInvalidOutputFallsBack(t *testing.T) {
	for _, out := range []string{
		"",
		"   ",
		"fix",                            // 1 word
		"a-b-c-d-e",                      // 5 words
		"!!!",                            // no letters
		"one",                            // still one word
		"alpha-beta-gamma-delta-epsilon", // 5 words
		"toolongwordwithoutanyhyphensatall",
	} {
		r := &stubRunner{out: out}
		name, err := ResolvePromptName(context.Background(), "task", r)
		require.NoError(t, err, out)
		requireCodename(t, name)
		require.NotEqual(t, out, name, out)
	}
}

func TestResolvePromptNameEmptyPromptOrNilRunner(t *testing.T) {
	name, err := ResolvePromptName(context.Background(), "   ", &stubRunner{out: "should-not-run"})
	require.NoError(t, err)
	requireCodename(t, name)

	name, err = ResolvePromptName(context.Background(), "real task", nil)
	require.NoError(t, err)
	requireCodename(t, name)
}

func TestResolvePromptNameHonoursParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &stubRunner{delay: time.Second, out: "too-late"}
	name, err := ResolvePromptName(ctx, "task", r)
	require.Error(t, err)
	requireCodename(t, name)
}

func TestSanitizeResolvedNameTrimsTo20(t *testing.T) {
	// 4 short words that exceed 20 chars once joined.
	got := sanitizeResolvedName("abcdef-ghijkl-mnopqr-st")
	require.LessOrEqual(t, len(got), maxResolvedNameLen)
	require.NotEmpty(t, got)
	require.GreaterOrEqual(t, strings.Count(got, "-")+1, 2)
}

func TestNameResolveArgCapsPrompt(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	arg := nameResolveArg(huge)
	require.Contains(t, arg, nameResolveInstruction)
	require.Less(t, len(arg), len(nameResolveInstruction)+2000+10)
}

func requireCodename(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, store.ValidateName(name), name)
	parts := strings.Split(name, "-")
	require.Len(t, parts, 2, name)
	require.Contains(t, adjectives, parts[0], name)
	require.Contains(t, nouns, parts[1], name)
}
