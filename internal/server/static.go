// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"net/http"
	"strings"
)

func (s *Server) fileExists(path string) bool {
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return true
	}
	f, err := s.static.Open(name)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// staticHandler serves the embedded frontend with an SPA fallback to index.html.
func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" || s.fileExists(r.URL.Path) {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: unknown paths render the single-page app.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}
