package daemon

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/srjn45/warden/internal/daemon/apidocs"
)

// registerAPIDocsRoutes wires the public OpenAPI surface (#43): Swagger UI at
// /api/docs, the raw spec at /api/docs/openapi.yaml, and the embedded UI assets
// under /api/docs/swagger-ui/. Like /healthz and the static SPA shell these are
// unauthenticated — the spec describes the API shape but holds no secrets, and
// keeping the docs public lets a remote browser load them and then prompt for a
// token (mirroring how the SPA shell is exposed). Gated by the api_docs config
// setting; when off every route 404s as if it weren't registered.
//
// Registered before registerStatic so chi's explicit /api/docs* routes win over
// the SPA catch-all.
func (s *Server) registerAPIDocsRoutes(r chi.Router) {
	r.Get("/api/docs", s.handleAPIDocsIndex)
	r.Get("/api/docs/openapi.yaml", s.handleOpenAPISpec)
	r.Get("/api/docs/swagger-ui/*", s.handleAPIDocsAsset)
}

// handleAPIDocsIndex serves the Swagger UI page (embedded index.html).
func (s *Server) handleAPIDocsIndex(w http.ResponseWriter, r *http.Request) {
	if !s.apiDocs {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(apidocs.SwaggerUI(), "index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "docs unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleOpenAPISpec serves the raw OpenAPI document.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if !s.apiDocs {
		http.NotFound(w, r)
		return
	}
	b, err := apidocs.Spec()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "spec unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(b)
}

// handleAPIDocsAsset serves the vendored Swagger UI static assets (css + JS
// bundles) from the embedded filesystem.
func (s *Server) handleAPIDocsAsset(w http.ResponseWriter, r *http.Request) {
	if !s.apiDocs {
		http.NotFound(w, r)
		return
	}
	ui := apidocs.SwaggerUI()
	// chi's "*" param is the path under /api/docs/swagger-ui/; clean it and reject
	// any traversal before touching the embedded FS.
	rest := strings.TrimPrefix(path.Clean("/"+chi.URLParam(r, "*")), "/")
	if rest == "" || rest == "." || strings.Contains(rest, "..") {
		http.NotFound(w, r)
		return
	}
	if st, err := fs.Stat(ui, rest); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, ui, rest)
}
