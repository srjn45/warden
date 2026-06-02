package daemon

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// freeAddr reserves an ephemeral port and returns its address. There is a tiny
// race between closing the listener and re-binding, but it is harmless in tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestListenAndServeShutsDownGracefully(t *testing.T) {
	srv := NewServer(newFakeStore(), &fakeLife{}, nil, time.Second, "")
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx, addr) }()

	// Wait until the server is actually accepting requests.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	cancel() // trigger graceful shutdown
	select {
	case err := <-errCh:
		require.NoError(t, err, "graceful shutdown should return without error")
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}
