package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoverMiddlewareReturns500OnPanic(t *testing.T) {
	// A handler panic must become a clean 500, not a dropped connection that
	// leaves the client with a reset and the daemon log full of stack traces.
	h := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	require.NoError(t, err, "the connection must not be dropped on a handler panic")
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}
