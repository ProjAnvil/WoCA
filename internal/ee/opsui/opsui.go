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
	"strings"
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
// routes (unknown non-API paths render the app shell).
func Handler() http.Handler {
	root := Root()
	files := http.FileServerFS(root)
	ix, err := fs.ReadFile(root, "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if _, err := fs.Stat(root, name); err != nil {
				// Client-side route: render the app shell.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(ix)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
