package updater

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSemverParsingAndComparison(t *testing.T) {
	v1, err := ParseSemver("9.25.0")
	require.NoError(t, err)
	v2, err := ParseSemver("v9.28.0")
	require.NoError(t, err)
	v3, err := ParseSemver("10.0.0-rc1")
	require.NoError(t, err)

	require.Equal(t, -1, v1.Compare(v2))
	require.Equal(t, 1, v2.Compare(v1))
	require.Equal(t, 0, v1.Compare(v1))
	require.Equal(t, -1, v2.Compare(v3))
	require.Equal(t, "9.25.0", v1.String())
}

func TestFetchManifest(t *testing.T) {
	m := Manifest{
		Version:        "9.28.0",
		SchemaVersion:  14,
		MinSchema:      11,
		MinUpgradeFrom: "9.25.0",
		Waypoint:       false,
		Breaking:       false,
		Notes:          []string{"strict integrity on open"},
		APICompat:      "Compatible with Hub API v1; Android app >= 1.4",
	}
	body, err := json.Marshal(m)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/manifest.json" {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	opts := Options{
		AssetBase:  srv.URL + "/download",
		HTTPClient: srv.Client(),
	}

	got, err := FetchManifest(opts, Release{Tag: "v9.28.0", Version: "9.28.0"})
	require.NoError(t, err)
	require.Equal(t, "9.28.0", got.Version)
	require.Equal(t, 14, got.SchemaVersion)
	require.Equal(t, "9.25.0", got.MinUpgradeFrom)
	require.Equal(t, "Compatible with Hub API v1; Android app >= 1.4", got.APICompat)
}
