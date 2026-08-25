// Package webdist embeds the built web frontend (web/dist) so binaries
// that serve the chat UI — the personal woca and the bridge execution
// plane — can ship it without external files. Headless API roles
// (woca-server, worker, ...) do not import this package; they serve no
// pages (the ops console UI is the separate woca-ops service).
//
// The package lives next to the built assets because go:embed only reaches
// the package's own directory tree; importers see it under the name webdist.
package webdist

import (
	"embed"
	"io/fs"
)

// dist embeds the SPA built into web/dist. Run `npm run build` in web/ (or
// `make web`) before compiling so the directory exists.
//
//go:embed all:dist
var dist embed.FS

// DistFS returns the embedded frontend filesystem rooted at web/dist.
func DistFS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
