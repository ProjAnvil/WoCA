// Package skillsvcui embeds the skill-asset console SPA (built from
// ee/skillsvc-web, synced by `make ee-web`). Only the woca-ops facade
// links it: woca-server hosts the skill platform's authoritative API
// (internal/ee/skillsvc) without any web pages, and the facade serves the
// SPA under /skillsvc/ while proxying every /api call back to the server
// — the same split as opsconsole/opsui.
package skillsvcui

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
// instead of a broken file server.
func Handler() http.Handler {
	if _, err := fs.Stat(spaFS, "spa/index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("skills console not built; run make ee-web"))
		})
	}
	return webfx.SPA(Root())
}
