// Package opsui embeds the tenant ops console SPA (built from
// ee/opsconsole-web, synced by `make ee-spa`). Only the woca-ops facade
// links it: woca-server hosts the console's authoritative API without any
// web pages, and the facade serves the SPA while proxying every /api call
// back to the server (product design: the console is a separate service
// that reaches woca-server through deployment-layer discovery).
package opsui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/projanvil/woca/internal/webfx"
)

//go:embed all:spa
var spaFS embed.FS

// Root returns the embedded SPA filesystem rooted at spa/.
func Root() fs.FS {
	sub, err := fs.Sub(spaFS, "spa")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler serves SPA assets and falls back to index.html for client-side
// routes (the shared webfx SPA contract).
func Handler() http.Handler { return webfx.SPA(Root()) }
