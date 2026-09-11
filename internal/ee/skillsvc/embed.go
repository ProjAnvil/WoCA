// Package skillsvc embeds the built SPA (skillsvc-web/dist) so the skills
// service ships the console with zero external files.
package skillsvc

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:spa
var spaFS embed.FS

// SpaHandler returns the HTTP handler serving the embedded console SPA.
// When the SPA was not built (fresh clone, no `make ee-web`), it answers
// 503 with a placeholder instead of a broken file server.
func SpaHandler() http.Handler {
	if _, err := fs.Stat(spaFS, "spa/index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("skills console not built; run make ee-web"))
		})
	}
	sub, err := fs.Sub(spaFS, "spa")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}
