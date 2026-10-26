// Package webdist embeds the built web frontend (web/dist) so binaries
// that serve the chat UI — the personal woca and the bridge execution
// plane — can ship it without external files. Headless API roles
// (woca-scheduler, worker, ...) do not import this package; they serve no
// pages (the ops console UI is the separate woca-ops service).
//
// The package lives next to the built assets because go:embed only reaches
// the package's own directory tree; importers see it under the name webdist.
package webdist

import (
	"embed"
	"io/fs"
)

// dist embeds the SPA built into web/dist. Run `bun run build` in web/ (or
// `make web`) before compiling so the directory holds a real build; when it
// does not (fresh clone), DistFS falls back to the stub page instead.
//
//go:embed all:dist
var dist embed.FS

// stub embeds the placeholder page served when web/dist holds no real
// build, so a bare `go build` still yields a working binary.
//
//go:embed all:stub
var stub embed.FS

// DistFS returns the embedded frontend filesystem rooted at web/dist, or
// the stub page when dist contains no real build (no index.html, or an
// index.html without built assets).
func DistFS() fs.FS {
	if _, err := fs.Stat(dist, "dist/index.html"); err == nil {
		if assets, _ := fs.Glob(dist, "dist/assets/*"); len(assets) > 0 {
			sub, err := fs.Sub(dist, "dist")
			if err != nil {
				panic(err)
			}
			return sub
		}
	}
	sub, err := fs.Sub(stub, "stub")
	if err != nil {
		panic(err)
	}
	return sub
}
