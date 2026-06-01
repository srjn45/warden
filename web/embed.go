// Package webui embeds the built Astro dashboard so the daemon can serve it.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// Dist returns the built UI filesystem rooted at the dist/ directory.
// Before `make ui` runs it contains only a placeholder; the daemon then
// serves an inline "UI not built" page (see internal/daemon/static.go).
func Dist() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err) // dist is always embedded; this is a programmer error
	}
	return sub
}
