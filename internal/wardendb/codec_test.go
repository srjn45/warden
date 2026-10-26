package wardendb

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLosslessDocumentRefusesEngineNumericPrecisionLoss(t *testing.T) {
	_, err := LosslessDocument(map[string]any{"value": json.Number("9007199254740993")})
	require.ErrorContains(t, err, "not losslessly representable")
	got, err := LosslessDocument(map[string]any{"value": json.Number("9007199254740991")})
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740991"), got["value"])
}
