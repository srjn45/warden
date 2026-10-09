package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/ownerlock"
)

// fakeDaemon serves the registry routes of the daemon over a real store,
// mirroring the status mapping of internal/daemon/strict_models.go.
func fakeDaemon(t *testing.T, st *backendstore.Store) string {
	t.Helper()
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, msg string) { write(w, code, map[string]string{"error": msg}) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		ms, err := st.ListModels(backendstore.ModelTier(r.URL.Query().Get("tier")))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if ms == nil {
			ms = []backendstore.ModelEntry{}
		}
		write(w, 200, ms)
	})
	mux.HandleFunc("POST /api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			BackendID, ModelID, DisplayName, Tier, QuotaScope string
			AutoAssign                                        bool
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		b.BackendID, _ = raw["backend_id"].(string)
		b.ModelID, _ = raw["model_id"].(string)
		b.DisplayName, _ = raw["display_name"].(string)
		b.Tier, _ = raw["tier"].(string)
		b.QuotaScope, _ = raw["quota_scope"].(string)
		b.AutoAssign, _ = raw["auto_assign"].(bool)
		err := st.AddModel(b.BackendID, b.ModelID, b.DisplayName, backendstore.ModelTier(b.Tier), b.AutoAssign, b.QuotaScope)
		if errors.Is(err, backendstore.ErrExists) {
			fail(w, 409, "exists")
			return
		} else if err != nil {
			fail(w, 400, err.Error())
			return
		}
		m, _ := st.GetModel(b.BackendID, b.ModelID)
		write(w, 201, m)
	})
	mux.HandleFunc("PUT /api/v1/models/{b}/{m}/tier", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		err := st.SetModelTier(r.PathValue("b"), r.PathValue("m"), backendstore.ModelTier(b["tier"]))
		if errors.Is(err, backendstore.ErrModelNotFound) {
			fail(w, 404, "nf")
			return
		} else if err != nil {
			fail(w, 400, err.Error())
			return
		}
		m, _ := st.GetModel(r.PathValue("b"), r.PathValue("m"))
		write(w, 200, m)
	})
	mux.HandleFunc("GET /api/v1/roles/tiers", func(w http.ResponseWriter, _ *http.Request) {
		ms, _ := st.ListRoleTiers()
		if ms == nil {
			ms = []backendstore.RoleTierMapping{}
		}
		write(w, 200, ms)
	})
	mux.HandleFunc("PUT /api/v1/roles/tiers/{role}", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if err := st.SetRoleTier(r.PathValue("role"), backendstore.ModelTier(b["tier"])); err != nil {
			fail(w, 400, err.Error())
			return
		}
		write(w, 200, backendstore.RoleTierMapping{RoleName: r.PathValue("role"), DefaultTier: backendstore.ModelTier(b["tier"])})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func stubStoreAt(t *testing.T, dir string) *backendstore.Store {
	t.Helper()
	st, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func setOwner(t *testing.T, owned bool) {
	t.Helper()
	orig := probeDataDirOwner
	probeDataDirOwner = func(string) (*ownerlock.OwnedError, error) {
		if !owned {
			return nil, nil
		}
		return &ownerlock.OwnedError{Dir: "x", Owner: &ownerlock.Owner{PID: 1, Kind: ownerlock.KindDaemon, Launch: ownerlock.LaunchSystemd}}, nil
	}
	t.Cleanup(func() { probeDataDirOwner = orig })
}

// run executes the same commands in offline mode (direct store) and in
// daemon-owned mode (API) against identically seeded stores and requires
// byte-identical output.
func TestRegistryCommandsIdenticalOfflineAndDaemon(t *testing.T) {
	cmds := [][]string{
		{"models", "list"},
		{"models", "list", "--json"},
		{"models", "list", "--by-tier"},
		{"models", "list", "--tier", "tier-1", "--json"},
		{"models", "tier", "claude", "sonnet", "tier-1"},
		{"models", "list", "--backend", "claude", "--json"},
		{"models", "add", "claude", "my-custom", "--tier", "tier-2", "--display", "Mine"},
		{"models", "add", "claude", "my-custom", "--tier", "tier-2"}, // already exists
		{"models", "tier", "claude", "nope", "tier-1"},               // not found
		{"models", "list", "--json"},
		{"agent", "role", "tier", "set", "implementer", "tier-3"},
		{"agent", "role", "tier", "list"},
		{"agent", "role", "tier", "list", "--json"},
	}
	run := func(daemon bool) []string {
		dir := t.TempDir()
		addr := "127.0.0.1:0"
		if daemon {
			addr = fakeDaemon(t, stubStoreAt(t, dir))
			orig := openBackendStore
			openBackendStore = func(*cobra.Command) (*backendstore.Store, error) {
				t.Error("daemon-owned mode must not open the store directly")
				return nil, errors.New("direct open")
			}
			t.Cleanup(func() { openBackendStore = orig })
		} else {
			orig := openBackendStore
			openBackendStore = func(*cobra.Command) (*backendstore.Store, error) { return backendstore.NewStore(dir) }
			t.Cleanup(func() { openBackendStore = orig })
		}
		setOwner(t, daemon)
		var outs []string
		for _, c := range cmds {
			out, err := runGit(t, addr, c...)
			if err != nil {
				out += "ERR: " + err.Error()
			}
			outs = append(outs, out)
		}
		return outs
	}
	off := run(false)
	dm := run(true)
	for i := range cmds {
		require.Equal(t, off[i], dm[i], "command %v", cmds[i])
	}
	require.Contains(t, off[7], "already exists")
	require.NotContains(t, off[4], "ERR")
	require.Contains(t, off[8], "ERR")
}

func TestDaemonOwnedDirIsNotRefusedAndUnreachableGivesGuidance(t *testing.T) {
	setOwner(t, true)
	// nothing listens on this port: daemon owns the dir but is unreachable.
	_, err := runGit(t, "127.0.0.1:1", "models", "list")
	require.Error(t, err)
	require.ErrorIs(t, err, ownerlock.ErrOwned)
	require.Contains(t, err.Error(), "systemctl --user status warden")
	require.Contains(t, err.Error(), "not reachable")
}
