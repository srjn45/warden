package daemon

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	webui "github.com/srajanpathak/agentctl/web"
)

const fallbackHTML = `<!doctype html><html><head><meta charset="utf-8"><title>agentctl</title></head>` +
	`<body><h1>agentctl</h1><p>UI not built. Run <code>make ui</code> (or <code>make release</code>) and restart the daemon.</p></body></html>`

// registerStatic serves the embedded Astro UI for any non-API GET path.
// It MUST be registered last so chi's explicit API routes take precedence.
func (s *Server) registerStatic(r chi.Router) {
	ui := webui.Dist()
	fileServer := http.FileServerFS(ui)
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		p := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if p == "" || p == "." {
			serveIndex(w, ui)
			return
		}
		if st, err := fs.Stat(ui, p); err != nil || st.IsDir() {
			serveIndex(w, ui) // SPA fallback for unknown client routes
			return
		}
		fileServer.ServeHTTP(w, req)
	})
}

func serveIndex(w http.ResponseWriter, ui fs.FS) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if b, err := fs.ReadFile(ui, "index.html"); err == nil {
		_, _ = w.Write(b)
		return
	}
	_, _ = w.Write([]byte(fallbackHTML))
}
