package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeArgs_Object(t *testing.T) {
	got, err := decodeArgs(json.RawMessage(`{"type":"development","prompt":"x"}`))
	require.NoError(t, err)
	require.Equal(t, "development", got["type"])
	require.Equal(t, "x", got["prompt"])
}

func TestDecodeArgs_StringifiedObject(t *testing.T) {
	// Some small models emit arguments as a JSON *string*, not an object.
	got, err := decodeArgs(json.RawMessage(`"{\"type\":\"docs\"}"`))
	require.NoError(t, err)
	require.Equal(t, "docs", got["type"])
}

func TestDecodeArgs_EmptyAndNull(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `""`} {
		got, err := decodeArgs(json.RawMessage(raw))
		require.NoError(t, err, "empty/null args are a valid no-arg call, not an error: %q", raw)
		require.NotNil(t, got, "always return a usable (possibly empty) map: %q", raw)
	}
}

func TestDecodeArgs_GarbageErrors(t *testing.T) {
	_, err := decodeArgs(json.RawMessage(`{not json`))
	require.Error(t, err, "un-parseable args must error so the loop can recover")
}

// Compile-time proof the concrete client will satisfy the seam (filled in Task 2).
var _ Chatter = (*Ollama)(nil)

// A trivial fake keeps Phase B unblocked before the real client lands.
type fakeChatter struct{ reply Reply }

func (f fakeChatter) Chat(context.Context, []Message, []ToolSchema) (Reply, error) {
	return f.reply, nil
}

func TestFakeChatterSatisfiesSeam(t *testing.T) {
	var c Chatter = fakeChatter{reply: Reply{Text: "ok"}}
	r, err := c.Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", r.Text)
}
