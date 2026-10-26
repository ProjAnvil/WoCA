// Package opsui embeds the tenant ops console SPA (built from
// ee/opsconsole-web, synced by `make ee-web`). Only the woca-ops facade
// links it: woca-scheduler hosts the console's authoritative API without any
// web pages, and the facade serves the SPA while proxying every /api call
// back to the server (product design: the console is a separate service
// that reaches woca-scheduler through deployment-layer discovery).
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
// routes (the shared webfx SPA contract). When the SPA was not built
// (fresh clone, no `make ee-web`), it answers 503 with a placeholder
// instead of panicking on the missing index.html.
func Handler() http.Handler {
	if _, err := fs.Stat(spaFS, "spa/index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("ops console not built; run make ee-web"))
		})
	}
	return webfx.SPA(Root())
}
