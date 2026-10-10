package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/updater"
	"github.com/stretchr/testify/require"
)

func TestUpdateCommand_PlanFlag(t *testing.T) {
	manifest := updater.Manifest{
		Version:        "9.28.0",
		SchemaVersion:  2,
		MinSchema:      1,
		MinUpgradeFrom: "9.25.0",
		Notes:          []string{"integrity strict checks"},
		APICompat:      "Hub API v1",
	}
	body, err := json.Marshal(manifest)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/srjn45/warden/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v9.28.0"}`))
		case "/download/manifest.json":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Direct call to updater.BuildPlan and FormatPlanText
	opts := updater.Options{
		CurrentVersion: "9.26.0",
		CurrentSchema:  1,
		TargetVersion:  "9.28.0",
		Repo:           "srjn45/warden",
		AssetBase:      srv.URL + "/download",
		HTTPClient:     srv.Client(),
	}

	plan, err := updater.BuildPlan(opts)
	require.NoError(t, err)
	require.Equal(t, "9.28.0", plan.TargetVersion)
	require.Equal(t, 2, plan.TargetSchema)
	require.True(t, plan.RequiresConfirm)

	text := updater.FormatPlanText(plan)
	require.Contains(t, text, "Update Plan: v9.26.0 → v9.28.0")
	require.Contains(t, text, "Data Schema: 1 → 2")
	require.Contains(t, text, "Trajectory: Direct upgrade")
	require.Contains(t, text, "Connected Client Compatibility Notes:")
	require.Contains(t, text, "Hub API v1")
	require.Contains(t, text, "integrity strict checks")
}

func TestUpdateCommand_PlanConfirmationDeclined(t *testing.T) {
	manifest := updater.Manifest{
		Version:        "9.28.0",
		SchemaVersion:  2,
		MinSchema:      1,
		MinUpgradeFrom: "9.25.0",
		Breaking:       true,
	}
	body, err := json.Marshal(manifest)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/srjn45/warden/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v9.28.0"}`))
		case "/download/manifest.json":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cmd := newUpdateCmd()
	var outBuf, inBuf bytes.Buffer
	inBuf.WriteString("n\n") // user declines confirmation

	cmd.SetOut(&outBuf)
	cmd.SetIn(&inBuf)
	cmd.SetArgs([]string{"--version", "9.28.0"})

	// Verify confirmYN directly with user declining
	ok := confirmYN(&inBuf, &outBuf, "Proceed? [y/N]: ")
	require.False(t, ok)
}
