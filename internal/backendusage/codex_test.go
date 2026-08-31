package backendusage

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type writeCloser struct{ io.Writer }

func (writeCloser) Close() error { return nil }

type readCloser struct{ io.Reader }

func (readCloser) Close() error { return nil }

func TestCodexAdapterPreservesAllLimitBuckets(t *testing.T) {
	rpc := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","method":"notice","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"account":{"type":"chatgpt","planType":"plus"},"requiresOpenaiAuth":false}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"rateLimitsByLimitId":{"codex":{"planType":"plus","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1788256800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":1788861600}},"review":{"planType":"plus","primary":{"usedPercent":100,"windowDurationMins":60},"limitReached":true}}}}`,
	}, "\n") + "\n"
	a := CodexAdapter{Now: func() time.Time { return time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC) }, Start: func(context.Context, string) (io.WriteCloser, io.ReadCloser, func() error, error) {
		return writeCloser{io.Discard}, readCloser{strings.NewReader(rpc)}, func() error { return nil }, nil
	}}
	got := a.Fetch(context.Background(), backendstore.Backend{ID: "codex", Installed: true, BinaryPath: "/synthetic/codex"})
	require.Equal(t, StatusRateLimited, got.Status)
	require.Len(t, got.Usage, 3)
	require.Equal(t, "codex:primary", got.Usage[0].ID)
	require.Equal(t, "codex", got.Usage[0].Scope)
	require.Equal(t, "codex primary", got.Usage[0].Label)
	require.Equal(t, float64(75), *got.Usage[0].RemainingPercent)
	require.Equal(t, "review:primary", got.Usage[2].ID)
	require.Equal(t, "review", got.Usage[2].Scope)
	require.Equal(t, "reached", *got.Usage[2].LimitState)
}

func TestCodexAdapterUnauthenticated(t *testing.T) {
	rpc := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"account\":null,\"requiresOpenaiAuth\":true}}\n"
	a := CodexAdapter{Start: func(context.Context, string) (io.WriteCloser, io.ReadCloser, func() error, error) {
		return writeCloser{io.Discard}, readCloser{strings.NewReader(rpc)}, func() error { return nil }, nil
	}}
	require.Equal(t, StatusUnauthenticated, a.Fetch(context.Background(), backendstore.Backend{ID: "codex", Installed: true}).Status)
}
