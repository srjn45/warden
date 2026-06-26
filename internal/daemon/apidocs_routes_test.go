package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/srjn45/warden/internal/daemon/apidocs"
)

// openapiDoc is the minimal structural view we assert against. The full document
// has far more; we only model what the drift-guard + validity checks read.
type openapiDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
		Schemas         map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openapiDoc {
	t.Helper()
	raw, err := apidocs.Spec()
	require.NoError(t, err, "embedded spec must load")
	var doc openapiDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc), "spec must parse as YAML")
	return doc
}

// TestOpenAPISpecIsValid asserts the embedded document is a well-formed OpenAPI
// 3.x spec with the load-bearing top-level sections present.
func TestOpenAPISpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	require.True(t, strings.HasPrefix(doc.OpenAPI, "3."), "must be OpenAPI 3.x, got %q", doc.OpenAPI)
	require.NotEmpty(t, doc.Info.Title)
	require.NotEmpty(t, doc.Info.Version)
	require.NotEmpty(t, doc.Paths, "spec must document at least one path")
	require.Contains(t, doc.Components.SecuritySchemes, "bearerAuth", "bearerAuth scheme must be documented")
	require.Contains(t, doc.Components.Schemas, "Session", "core Session schema must be present")
	require.Contains(t, doc.Components.Schemas, "Error", "error envelope schema must be present")

	// A representative set of known endpoints must be documented. The data API
	// lives under /api/v1; /healthz and /api/docs stay at the root.
	for _, p := range []string{
		"/healthz", "/api/v1/sessions", "/api/v1/sessions/{id}", "/api/v1/spawn",
		"/api/v1/git/commit", "/api/v1/pipelines", "/api/v1/snapshots", "/api/docs",
	} {
		require.Contains(t, doc.Paths, p, "spec must document %s", p)
	}
}

// chiRoutes walks the registered mux and returns the concrete (non-wildcard)
// routes as method-agnostic path strings. Wildcard routes (the SPA catch-all and
// the swagger-ui asset subtree) carry no documentable schema and are skipped.
func chiRoutes(t *testing.T, srv *Server) map[string]bool {
	t.Helper()
	routes := map[string]bool{}
	mux, ok := srv.router().(chi.Routes)
	require.True(t, ok, "router must be a chi.Routes")
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "*") {
			return nil
		}
		// chi reports the trailing slash on a bare group root; normalize none here
		// since every registered route is slash-free at its tail.
		routes[route] = true
		return nil
	})
	require.NoError(t, err)
	return routes
}

// TestSpecMatchesRoutes is a route-presence smoke test. Since the strict server
// is generated from openapi.yaml, the request/response *schemas* are enforced by
// the compiler (every operationId becomes an interface method the daemon must
// implement) and the committed api.gen.go is kept in lockstep with the spec by
// the `go generate` CI guard. What this test still adds is parity for the
// hand-registered routes that sit outside strict generation — /healthz, the
// /api/docs surface, the SSE stream and the attach socket: every concrete route
// the daemon registers must have a matching spec path, and every spec path must
// correspond to a real route.
func TestSpecMatchesRoutes(t *testing.T) {
	doc := loadSpec(t)
	srv := &Server{apiDocs: true}
	routes := chiRoutes(t, srv)

	specPaths := map[string]bool{}
	for p := range doc.Paths {
		specPaths[p] = true
	}

	var missingFromSpec, missingFromRoutes []string
	for r := range routes {
		if !specPaths[r] {
			missingFromSpec = append(missingFromSpec, r)
		}
	}
	for p := range specPaths {
		if !routes[p] {
			missingFromRoutes = append(missingFromRoutes, p)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(missingFromRoutes)
	require.Empty(t, missingFromSpec, "routes registered but NOT documented in openapi.yaml: %v", missingFromSpec)
	require.Empty(t, missingFromRoutes, "paths in openapi.yaml with NO matching daemon route: %v", missingFromRoutes)
}

// TestAPIDocsServed asserts the docs surface is wired and public (no auth) when
// enabled: the Swagger UI page returns HTML referencing the spec, the raw spec
// route serves the YAML document, and the vendored assets are reachable.
func TestAPIDocsServed(t *testing.T) {
	srv := &Server{store: newFakeStore(), apiDocs: true}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	t.Run("swagger ui page", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/docs")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
		body, _ := io.ReadAll(resp.Body)
		require.Contains(t, string(body), "/api/docs/openapi.yaml", "UI must point at the embedded spec")
		require.Contains(t, string(body), "swagger-ui", "UI must load the embedded swagger-ui assets")
	})

	t.Run("raw spec", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/docs/openapi.yaml")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		require.Contains(t, string(body), "openapi: 3", "raw route must serve the OpenAPI document")
	})

	t.Run("vendored asset", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/docs/swagger-ui/swagger-ui.css")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestAPIDocsDisabled asserts the api_docs gate hides the surface entirely.
func TestAPIDocsDisabled(t *testing.T) {
	srv := &Server{store: newFakeStore(), apiDocs: false}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	for _, p := range []string{"/api/docs", "/api/docs/openapi.yaml", "/api/docs/swagger-ui/swagger-ui.css"} {
		resp, err := http.Get(ts.URL + p)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode, "gated route %s must 404 when disabled", p)
	}
}

// TestAPIDocsPublic asserts the docs surface stays reachable even when bearer
// auth is on — like /healthz and the SPA shell, it must not be token-gated.
func TestAPIDocsPublic(t *testing.T) {
	srv := &Server{store: newFakeStore(), apiDocs: true, authToken: "secret"}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/docs/openapi.yaml")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "docs spec must be public even with auth enabled")
}
