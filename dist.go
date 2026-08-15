// Package woca is the module root package. It embeds the built web frontend so
// a single `woca` binary can serve the UI without any external files.
package woca

import (
	"embed"
	"io/fs"
)

// dist embeds the built SPA under web/dist. Run `npm run build` in web/ (or
// `make web`) before compiling so the directory exists.
//
//go:embed web/dist
var dist embed.FS

// DistFS returns the embedded frontend filesystem rooted at web/dist.
func DistFS() fs.FS {
	sub, err := fs.Sub(dist, "web/dist")
	if err != nil {
		panic(err)
	}
	return sub
}
