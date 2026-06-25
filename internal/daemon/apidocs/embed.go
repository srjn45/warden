// Package apidocs embeds warden's OpenAPI specification and a pinned, offline
// copy of Swagger UI so the daemon can serve interactive API docs at /api/docs
// without any runtime CDN dependency (#43). It mirrors the web/ embed pattern:
// a go:embed FS plus small accessors the daemon's route handlers read from.
package apidocs

import (
	"embed"
	"io/fs"
)

//go:embed openapi.yaml
//go:embed swagger-ui
var files embed.FS

// SwaggerUIVersion is the pinned swagger-ui-dist release vendored under
// swagger-ui/. Bump it (and re-vendor the assets) deliberately.
const SwaggerUIVersion = "5.17.14"

// Spec returns the raw OpenAPI document bytes.
func Spec() ([]byte, error) {
	return files.ReadFile("openapi.yaml")
}

// SwaggerUI returns the embedded Swagger UI asset filesystem, rooted at the
// swagger-ui/ directory (index.html, the css, and the JS bundles).
func SwaggerUI() fs.FS {
	sub, err := fs.Sub(files, "swagger-ui")
	if err != nil {
		panic(err) // swagger-ui is always embedded; a failure here is a build error
	}
	return sub
}
