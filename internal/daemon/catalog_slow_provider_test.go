package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// blockedCatalogBackend is a registered backend whose catalog adapter blocks
// until released, standing in for a hung provider CLI.
type blockedCatalogBackend struct {
	agentbackend.Backend
	entered chan struct{}
	release chan struct{}
}

func (b blockedCatalogBackend) ResolveScope(context.Context) (agentbackend.CatalogScope, error) {
	return agentbackend.CatalogScope{}, nil
}

func (b blockedCatalogBackend) DiscoverModels(context.Context, agentbackend.CatalogScope) agentbackend.CatalogObservation {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return agentbackend.CatalogObservation{}
}

// TestSlowCatalogProviderDoesNotBlockSessionsApprovalsOrTree blocks a provider
// adapter mid-discovery and verifies the daemon still serves sessions,
// approvals and the project tree. The adapter is released only afterwards.
func TestSlowCatalogProviderDoesNotBlockSessionsApprovalsOrTree(t *testing.T) {
	orig, err := agentbackend.Get("cursor")
	require.NoError(t, err)
	blocked := blockedCatalogBackend{Backend: orig, entered: make(chan struct{}, 1), release: make(chan struct{})}
	agentbackend.Register(blocked)
	t.Cleanup(func() { agentbackend.Register(orig) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = blocked.DiscoverModels(context.Background(), agentbackend.CatalogScope{})
	}()
	select {
	case <-blocked.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked adapter never entered discovery")
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(blocked.release)
		}
	}
	t.Cleanup(func() { release(); <-done })

	pane := "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"
	fs := newFakeStore()
	fs.data["a1"] = &agentstore.Agent{ID: "a1", TmuxSession: "a1", Status: store.StatusWaitingForInput}
	fl := &fakeLife{output: pane}
	ts := httptest.NewServer((&Server{store: fs, life: fl, approvals: true}).router())
	defer ts.Close()
	fp := approval.Fingerprint([]string{"Yes", "No"})
	body, _ := json.Marshal(ApproveRequest{Option: 1, Fingerprint: fp})

	type result struct {
		name   string
		status int
		err    error
	}
	results := make(chan result, 3)
	client := &http.Client{Timeout: 3 * time.Second}
	do := func(name string, req func() (*http.Response, error)) {
		go func() {
			resp, err := req()
			r := result{name: name, err: err}
			if err == nil {
				r.status = resp.StatusCode
				_ = resp.Body.Close()
			}
			results <- r
		}()
	}
	do("sessions", func() (*http.Response, error) { return client.Get(ts.URL + "/api/v1/sessions") })
	do("tree", func() (*http.Response, error) { return client.Get(ts.URL + "/api/v1/tree") })
	do("approve", func() (*http.Response, error) {
		return client.Post(ts.URL+"/api/v1/sessions/a1/approve", "application/json", bytes.NewReader(body))
	})
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			require.NoError(t, r.err, r.name)
			require.Equal(t, http.StatusOK, r.status, r.name)
		case <-time.After(5 * time.Second):
			t.Fatal("daemon request blocked behind slow catalog provider")
		}
	}
	require.Equal(t, "1", fl.lastKey, "approval must have been delivered while provider was blocked")
	select {
	case <-done:
		t.Fatal("provider should still be blocked until explicitly released")
	default:
	}
}

// forbiddenDiscoveryBackend records any catalog or live-menu call. The daemon
// must never reach it on its own.
type forbiddenDiscoveryBackend struct {
	agentbackend.Backend
	called chan string
}

func (b forbiddenDiscoveryBackend) note(what string) {
	select {
	case b.called <- b.ID() + ":" + what:
	default:
	}
}
func (b forbiddenDiscoveryBackend) ListModels() ([]string, bool) {
	b.note("ListModels")
	return nil, false
}
func (b forbiddenDiscoveryBackend) ResolveScope(context.Context) (agentbackend.CatalogScope, error) {
	b.note("ResolveScope")
	return agentbackend.CatalogScope{}, nil
}
func (b forbiddenDiscoveryBackend) DiscoverModels(context.Context, agentbackend.CatalogScope) agentbackend.CatalogObservation {
	b.note("DiscoverModels")
	return agentbackend.CatalogObservation{}
}

// TestDaemonBootAndTicksNeverRunCatalogDiscovery boots the real server loop with
// every registered backend wired to a discovery tripwire, lets background
// watchers tick and serves ordinary reads, then asserts no provider was touched.
func TestDaemonBootAndTicksNeverRunCatalogDiscovery(t *testing.T) {
	called := make(chan string, 16)
	for _, id := range agentbackend.IDs() {
		orig, err := agentbackend.Get(id)
		require.NoError(t, err)
		agentbackend.Register(forbiddenDiscoveryBackend{Backend: orig, called: called})
		t.Cleanup(func() { agentbackend.Register(orig) })
	}
	fs := newFakeStore()
	fs.data["a1"] = &agentstore.Agent{ID: "a1", Status: store.StatusWorking}
	srv := NewServer(fs, &fakeLife{}, nil, 20*time.Millisecond, false, nil, nil, nil)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx, addr) }()
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
	for _, p := range []string{"/api/v1/sessions", "/api/v1/tree"} {
		resp, err := http.Get("http://" + addr + p)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond) // several 20ms background sweep ticks
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatal("ListenAndServe did not return")
	}
	select {
	case c := <-called:
		t.Fatalf("daemon ran provider catalog discovery on its own: %s", c)
	default:
	}
}
