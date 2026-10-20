package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
)

func postProposal(t *testing.T, url string, body map[string]any) (*http.Response, backendstore.CatalogProposal) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url+"/api/v1/model-catalog/proposals", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	var p backendstore.CatalogProposal
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	}
	return resp, p
}

func previewBody(fp string) map[string]any {
	b := map[string]any{
		"aicli": "codex",
		"observation": map[string]any{
			"source": "live_command", "completeness": "complete",
			"models": []map[string]any{{"model_id": "m-1"}},
		},
	}
	if fp != "" {
		b["scope_fp"] = fp
	}
	return b
}

func TestPreviewModelCatalogHonoursOpaqueScopeFingerprint(t *testing.T) {
	srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
	defer srv.Close()

	a := backendstore.CatalogScope{AiCli: "codex", AccountRef: "acct-a", Profile: "p"}.ScopeFP()
	b := backendstore.CatalogScope{AiCli: "codex", AccountRef: "acct-b", Profile: "p"}.ScopeFP()
	require.NotEqual(t, a, b)

	resp, pa := postProposal(t, srv.URL, previewBody(a))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, pb := postProposal(t, srv.URL, previewBody(b))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, pn := postProposal(t, srv.URL, previewBody(""))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, a, pa.ScopeFP)
	require.Equal(t, b, pb.ScopeFP)
	require.NotEqual(t, pa.ScopeFP, pb.ScopeFP, "two accounts of one aicli stay isolated")
	require.Equal(t, backendstore.CatalogScope{AiCli: "codex"}.ScopeFP(), pn.ScopeFP, "omitted fingerprint keeps the aicli-only scope")
}

func TestPreviewModelCatalogRejectsMalformedScopeFingerprint(t *testing.T) {
	srv, _ := lifeServerBackends(t, newFakeStore(), &fakeLife{})
	defer srv.Close()

	for _, fp := range []string{"acct-a", "sha256:zz", "sha256:dd9208115a23db829577df2", "sha256:dd9208115a23db829577df255"} {
		resp, _ := postProposal(t, srv.URL, previewBody(fp))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, fp)
	}
}
