package webfx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Swap is an atomic handler slot for surfaces that must answer from the
// moment the port is bound: serve a placeholder while assembly completes
// (agent build, config pull), then Store the real handler. Loads are
// wait-free; concurrent requests during a swap see either handler, never
// a torn one.
type Swap struct {
	cur atomic.Pointer[http.Handler]
}

// NewSwap returns a Swap serving placeholder until Store replaces it.
func NewSwap(placeholder http.Handler) *Swap {
	s := &Swap{}
	s.cur.Store(&placeholder)
	return s
}

// Store atomically replaces the served handler.
func (s *Swap) Store(h http.Handler) { s.cur.Store(&h) }

// ServeHTTP serves the currently installed handler.
func (s *Swap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.cur.Load()).ServeHTTP(w, r)
}

// Run binds Options.Addr (wrapping the listener with Options.TLS when
// set), serves until ctx is cancelled, then shuts down gracefully within
// Options.Grace. A listener failure is returned immediately — fail fast,
// no placeholder here.
func (r *Router) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.opts.Addr, err)
	}
	if r.opts.TLS != nil {
		cert, err := tls.LoadX509KeyPair(r.opts.TLS.CertFile, r.opts.TLS.KeyFile)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("tls: load keypair: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		})
	}
	return r.Serve(ctx, ln)
}

// Serve drives an already-bound listener with the router's handler until
// ctx is cancelled, then shuts down gracefully. Callers that own binding
// (edge's public-bind policy checks) use this entry.
func (r *Router) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           r.Handler(),
		ReadHeaderTimeout: r.readHeaderTimeout(),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), r.grace())
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (r *Router) grace() time.Duration {
	if r.opts.Grace > 0 {
		return r.opts.Grace
	}
	return defaultGrace
}

func (r *Router) readHeaderTimeout() time.Duration {
	if r.opts.ReadHeaderTimeout > 0 {
		return r.opts.ReadHeaderTimeout
	}
	return defaultReadHeaderTimout
}
