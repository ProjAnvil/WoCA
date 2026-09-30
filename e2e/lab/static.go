package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// runStatic serves the built webdist (web/dist) on the fixed static port —
// the host page for native-wizard tests, which inject the wails
// window.go shim per-test. Run from the repo root.
func runStatic() {
	addr := "127.0.0.1:13714"
	dist := filepath.Join("web", "dist")
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		fmt.Fprintln(os.Stderr, "lab/static: web/dist has no build — run `make web` first")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__e2e/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	fs := http.FileServer(http.Dir(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Never cache during a test run: specs reload the same assets
		// across builds.
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab/static: listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Printf("lab/static ready on http://%s\n", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "lab/static: serve: %v\n", err)
		os.Exit(1)
	}
}
