// Package webgui manages the embedded web UI as an on-demand service that can
// be started and stopped from the console (the /web command), in the spirit of
// Claude Code's `claude server` (Direct Connect).
package webgui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/server"
)

// DefaultPort is the port the web GUI listens on when none is given.
const DefaultPort = 3484

// DefaultAddr is the default listen address (loopback only).
const DefaultAddr = "127.0.0.1:3484"

// Manager starts and stops the embedded web server on demand. It is safe for
// concurrent use.
type Manager struct {
	cfg func(addr string) *server.Server // builds a fresh server per start

	mu       sync.Mutex
	srv      *http.Server
	listener net.Listener
	addr     string
}

// New creates a Manager around a server factory. The factory is invoked on
// every Start so each server instance gets fresh run state.
func New(cfg server.Config) *Manager {
	return &Manager{cfg: func(string) *server.Server { return server.New(cfg) }}
}

// Running reports whether the web GUI is currently serving, returning its
// address when it is.
func (m *Manager) Running() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr, m.srv != nil
}

// Start begins serving the web GUI. An empty addr falls back to DefaultAddr.
// It returns the URL the UI is reachable at. Starting while already running,
// or binding a taken port, returns an error.
func (m *Manager) Start(addr string) (string, error) {
	if addr == "" {
		addr = DefaultAddr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv != nil {
		return "", fmt.Errorf("web gui already running at http://%s (use /web off first)", m.addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("web gui: listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: m.cfg(addr).Handler()}
	m.srv = srv
	m.listener = ln
	m.addr = ln.Addr().String()
	go func() {
		_ = srv.Serve(ln)
	}()
	return "http://" + m.addr, nil
}

// Stop shuts the web GUI down gracefully (up to 3s). It is a no-op when not
// running.
func (m *Manager) Stop() error {
	m.mu.Lock()
	srv := m.srv
	m.srv = nil
	m.listener = nil
	addr := m.addr
	m.addr = ""
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web gui: stop %s: %w", addr, err)
	}
	return nil
}
