// Package webdist embeds the built web frontend (web/dist) so binaries that
// serve the UI — the personal woca and the server/gateway roles — can ship it
// without external files. Entries that must not carry the SPA (worker,
// executor, ...) simply do not import this package.
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
