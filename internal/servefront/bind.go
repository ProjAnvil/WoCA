package servefront

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
)

// bindEarly is ServeWithEdition's listener phase: bind the port and answer
// /api/health before the agent build runs (the build's network calls can
// hang for seconds under a VPN, which used to leave the port unbound and
// the webd daemon timed out on its 5s readiness probe), terminate direct
// TLS when configured (product design §5.3 path C), and write the ready
// file for native-client embedding. Everything except health 503s until
// the real handler is swapped in through the returned pointer. The serve
// goroutine is already running when bindEarly returns nil.
func bindEarly(addr string, obs Observability) (*http.Server, string, chan error, *atomic.Pointer[http.Handler], error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	// atomic.Pointer, not atomic.Value: the swapped-in concrete handler
	// types differ (http.HandlerFunc vs *http.ServeMux) and atomic.Value
	// panics on inconsistent types.
	var handler atomic.Pointer[http.Handler]
	starting := http.Handler(http.HandlerFunc(startingHandler))
	if obs != nil {
		// Probes must answer from the first moment the port is bound.
		starting = obs.Wrap(starting)
	}
	handler.Store(&starting)
	hs := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load()).ServeHTTP(w, r)
	})}
	serveErr := make(chan error, 1)
	// Direct TLS (product design §5.3 path C): WOCA_TLS_CERT/WOCA_TLS_KEY
	// terminate TLS on the business listener itself — for public bridges
	// without a reverse proxy. (The reverse-proxy paths keep using plain
	// HTTP on loopback behind Caddy/Tailscale.)
	scheme := "http"
	if tlsCert, tlsKey := os.Getenv("WOCA_TLS_CERT"), os.Getenv("WOCA_TLS_KEY"); tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			_ = hs.Close()
			return nil, scheme, nil, nil, fmt.Errorf("tls: load keypair: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		})
		scheme = "https"
	}
	// Ready file (native client embedding): when WOCA_READY_FILE is set,
	// the bound URL lands there atomically right after listen — the
	// embedding process polls this file to discover the random port.
	if f := os.Getenv("WOCA_READY_FILE"); f != "" && scheme == "http" {
		_ = os.WriteFile(f, []byte("http://"+ln.Addr().String()), 0o600)
	}
	go func() { serveErr <- hs.Serve(ln) }()
	return hs, scheme, serveErr, &handler, nil
}
