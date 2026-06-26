package daemon

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStaticServesIndexAtRoot(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "warden")
}

func TestStaticDoesNotShadowAPI(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/sessions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

// The SPA owns the bare client-route paths (/metrics, /pipelines, …): now that
// the data API lives under /api/v1, a browser navigation to one of those URLs
// must fall through to the app shell (HTML) instead of hitting a JSON endpoint.
func TestStaticServesSPARouteThatCollidedWithAPI(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	for _, p := range []string{"/metrics", "/pipelines"} {
		resp, err := http.Get(ts.URL + p)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "text/html", "%s must serve the SPA shell", p)
	}
}

func TestStaticSPAFallback(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/some/client/route")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}
