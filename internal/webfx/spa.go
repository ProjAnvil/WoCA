package webfx

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

// SPA serves a single-page app filesystem with an index.html fallback for
// client-side routes: GET/HEAD only, /api/ paths never fall back (an SPA
// miss under /api is a 404, not the app shell), and any other unknown path
// renders index.html. One implementation replaces the per-role copies that
// had already converged on these exact semantics.
func SPA(root fs.FS) http.Handler {
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

// Healthz answers the infrastructure liveness probe: 200 with the fixed
// payload every woca role serves today. Deliberately static — health
// aggregation (readiness) is a separate concern this framework does not
// own. The output is byte-identical to ee/obs's handler so probes see the
// same body regardless of which layer serves it.
func Healthz(service string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": service})
	})
}
