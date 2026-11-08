// Package assets embeds the SPA frontend (index.html + app.js) into the
// binary. Embedding eliminates the PUBLIC_DIR path dependency that made
// deployment fragile: a fnOS upgrade could wipe or relocate the www
// directory, leaving the app serving 404s. The frontend is now
// self-contained; an on-disk PublicDir is still honoured when present
// (useful for local development) but is only a fallback.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed www
var wwwFS embed.FS

// Frontend returns the embedded frontend rooted at the www directory
// contents (i.e. "index.html", "app.js", ...). The fs.Sub call strips the
// "www/" prefix so callers can use the path directly.
func Frontend() fs.FS {
	sub, err := fs.Sub(wwwFS, "www")
	if err != nil {
		// Unreachable: the embed directive guarantees www exists.
		panic("assets: embedded www directory missing: " + err.Error())
	}
	return sub
}
