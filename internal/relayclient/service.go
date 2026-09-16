package relayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
)

// Service owns the relayclient lifecycle inside `woca serve`: it reads
// the persisted (or environment) configuration, runs the tunnel against
// the server handler once available, and exposes the local status
// surface (design §3.2).
type Service struct {
	mu         sync.Mutex
	handler    http.Handler
	st         *settings.Store
	client     *Client
	cancel     context.CancelFunc
	lastStatus *Status
}

// NewService builds an idle service.
func NewService() *Service { return &Service{} }

// SetHandler receives the fully-built server handler (the servefront
// Edition.Handler seam) — the tunnel executes against exactly this
// stack, the same code path as local web traffic (design §3.2).
func (s *Service) SetHandler(h http.Handler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// SetSettings receives the settings store (the servefront
// Edition.OnSettings seam) for the status surface.
func (s *Service) SetSettings(st *settings.Store) {
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
}

// Run starts the tunnel when a relay is configured (environment wins
// over persisted settings) and blocks until ctx ends or the tunnel hits
// a terminal state. Unconfigured is a no-op (local-only posture).
func (s *Service) Run(ctx context.Context, st *settings.Store, log zerolog.Logger) {
	url, key := ResolveConfig(st)
	if url == "" || key == "" {
		return
	}
	s.mu.Lock()
	for s.handler == nil {
		// SetHandler runs before Edition.Start in the serve path; this
		// wait is defensive against wiring drift.
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		s.mu.Lock()
	}
	handler := s.handler
	runCtx, cancel := context.WithCancel(ctx)
	client := New(Config{URL: url, Key: key, Handler: handler}, log)
	s.client = client
	s.cancel = cancel
	s.mu.Unlock()

	log.Info().Str("url", url).Msg("relay: connecting bridge tunnel")
	client.Run(runCtx)

	final := client.Status()
	s.mu.Lock()
	s.lastStatus = &final
	s.client = nil
	s.cancel = nil
	s.mu.Unlock()
}

// Disconnect stops the live tunnel (`woca relay disconnect` / the local
// API). The next serve start reconnects from the persisted config.
func (s *Service) Disconnect() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Status merges the live tunnel snapshot with the persisted
// configuration.
func (s *Service) Status() Status {
	s.mu.Lock()
	st := s.st
	client := s.client
	last := s.lastStatus
	s.mu.Unlock()

	url, key := ResolveConfig(st)
	base := Status{Configured: url != "" && key != ""}
	base.KeyPrefix = key
	if len(base.KeyPrefix) > 8 {
		base.KeyPrefix = base.KeyPrefix[:8]
	}
	if client != nil {
		cur := client.Status()
		cur.Configured = base.Configured
		cur.KeyPrefix = base.KeyPrefix
		return cur
	}
	if last != nil {
		out := *last
		out.Configured = base.Configured
		out.KeyPrefix = base.KeyPrefix
		out.Connected = false
		return out
	}
	return base
}

// rejectRemote guards the loopback-trust local surface against tunnel
// traffic: the tunnel forwards every /api/* request to the full handler,
// so without this check a session-authenticated remote client could POST
// /api/relay/disconnect and kill the bridge's own tunnel (self-lockout,
// recoverable only with local access). The marker is forgery-proof —
// execute() strips every X-Woca-* header from parked requests before
// injecting it itself (design §6.4).
func rejectRemote(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(remoteHeader) == "" {
		return false
	}
	http.Error(w, "relay local surface: not reachable through the tunnel", http.StatusForbidden)
	return true
}

// StatusHandler serves GET /api/relay/status (local surface — the CLI
// and the local web status page read it, design §3.2).
func (s *Service) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectRemote(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Status())
	})
}

// DisconnectHandler serves POST /api/relay/disconnect (stops the live
// tunnel; the persisted configuration is cleared by the CLI).
func (s *Service) DisconnectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectRemote(w, r) {
			return
		}
		s.Disconnect()
		w.WriteHeader(http.StatusNoContent)
	})
}
