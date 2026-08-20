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
func SpaHandler() http.Handler {
	sub, err := fs.Sub(spaFS, "spa")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}
