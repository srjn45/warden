package cli

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenGenerateCommand(t *testing.T) {
	cmd := newTokenGenerateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	tok := strings.TrimSpace(out.String())
	require.Len(t, tok, 64, "generate must print a 32-byte hex token")
	_, err := hex.DecodeString(tok)
	require.NoError(t, err, "printed token must be valid hex")
}
