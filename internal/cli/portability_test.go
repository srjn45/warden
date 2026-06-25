package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// runCLIStdin drives a wd subcommand against a stub daemon at addr with the
// given stdin, returning stdout+stderr.
func runCLIStdin(t *testing.T, addr, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	full := append(args, "--addr", addr, "--config", t.TempDir()+"/none.yaml")
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

// portabilityStub serves the export read paths (/sessions, /history) from canned
// data and records the body posted to /import.
func portabilityStub(t *testing.T, active, closed []*store.Session) (addr string, imported *store.Export) {
	t.Helper()
	var got store.Export
	imported = &got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": active})
		case r.URL.Path == "/history":
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": closed})
		case r.URL.Path == "/import":
			_ = json.NewDecoder(r.Body).Decode(&got)
			res := store.ImportResult{}
			for _, s := range got.Sessions {
				res.Imported = append(res.Imported, s.ID)
			}
			_ = json.NewEncoder(w).Encode(res)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), imported
}

func TestExportEnvelopeShape(t *testing.T) {
	active := []*store.Session{{ID: "A-1", Subject: "live"}}
	closed := []*store.Session{{ID: "A-9", Subject: "archived"}}
	addr, _ := portabilityStub(t, active, closed)

	// Default export covers only active sessions.
	out, err := runCLIStdin(t, addr, "", "export")
	require.NoError(t, err)
	var env store.Export
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	require.Equal(t, store.ExportVersion, env.Version)
	require.False(t, env.ExportedAt.IsZero())
	require.Len(t, env.Sessions, 1)
	require.Equal(t, "A-1", env.Sessions[0].ID)

	// --all folds in the archived records too.
	out, err = runCLIStdin(t, addr, "", "export", "--all")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	require.Len(t, env.Sessions, 2)
}

func TestExportImportRoundTrip(t *testing.T) {
	active := []*store.Session{{ID: "A-1", Subject: "live"}, {ID: "A-2", Subject: "other"}}
	addr, imported := portabilityStub(t, active, nil)

	dump, err := runCLIStdin(t, addr, "", "export")
	require.NoError(t, err)

	out, err := runCLIStdin(t, addr, dump, "import")
	require.NoError(t, err)
	require.Contains(t, out, "imported 2")

	// The bytes export emitted decode back into the same records the import POSTed.
	require.Len(t, imported.Sessions, 2)
	require.Equal(t, "A-1", imported.Sessions[0].ID)
	require.Equal(t, "A-2", imported.Sessions[1].ID)
}

func TestImportRejectsMalformedJSON(t *testing.T) {
	addr, _ := portabilityStub(t, nil, nil)
	_, err := runCLIStdin(t, addr, "{not json", "import")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse import JSON")
}
