package relay

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/webfx"
)

// Relay is the public surface assembly: auth routes, the SPA, the health
// probe, and the tunnel forwarding plane (parked requests, generation
// leases, bridge endpoints).
type Relay struct {
	keys        *KeyStore
	auth        *AuthService
	spa         http.Handler
	behindProxy bool
	log         zerolog.Logger

	park   *ParkingQueue
	gens   *GenerationRegistry
	tunnel *TunnelService
	audit  *AuditLog // attached by the revocation task
}

// NewRelay builds the full surface: auth + SPA + health + the tunnel
// plane (parking queue, generation registry, tunnel endpoints).
func NewRelay(keys *KeyStore, spaFS fs.FS, behindProxy bool, log zerolog.Logger) *Relay {
	park := NewParkingQueue(ParkCapacity)
	gens := NewGenerationRegistry()
	return &Relay{
		keys:        keys,
		auth:        NewAuthService(keys, behindProxy, log),
		spa:         webfx.SPA(spaFS),
		behindProxy: behindProxy,
		log:         log,
		park:        park,
		gens:        gens,
		tunnel:      NewTunnelService(keys, park, gens, log),
	}
}

// Handler is the full public surface. Anonymous reach is exactly the
// design §7.2 whitelist: SPA assets, /api/health, /api/auth/login.
// /tunnel/ endpoints enforce Bearer-key + generation auth inside their
// own handlers; every other /api/ path requires a session once the
// forwarding plane is attached.
func (rl *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-relay"})
	})
	mux.HandleFunc("POST /api/auth/login", rl.auth.HandleLogin)
	mux.HandleFunc("POST /api/auth/logout", rl.auth.HandleLogout)
	mux.HandleFunc("GET /api/auth/me", rl.auth.HandleMe)
	mux.HandleFunc("GET /tunnel/poll", rl.tunnel.HandlePoll)
	mux.HandleFunc("POST /tunnel/respond/{id}", rl.tunnel.HandleRespond)
	mux.HandleFunc("/", rl.route)
	return mux
}

// route: authenticated /api/ traffic forwards through the tunnel; the SPA
// gets everything else (and 404s unknown /api/ paths itself — but with
// the tunnel attached, every /api/ path requires a session first).
func (rl *Relay) route(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") && rl.tunnel != nil {
		sess, err := rl.auth.SessionFromRequest(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		rl.forward(w, r, sess)
		return
	}
	rl.spa.ServeHTTP(w, r)
}

// CheckBind enforces the design §7.2 fail-closed red line: a public bind
// without TLS refuses to start unless --insecure was passed explicitly
// (development only). --behind-proxy declares a trusted TLS-terminating
// front (Caddy / CF Tunnel / Tailscale Funnel); the relay then serves
// plain HTTP and should bind loopback or an internal interface — a public
// behind-proxy bind logs a warning instead of failing, because some
// fronts legitimately reach the relay over a private-looking address.
func CheckBind(addr string, tlsOn, insecure, behindProxy bool, log zerolog.Logger) error {
	if tlsOn || !isPublicBind(addr) {
		return nil
	}
	if insecure {
		log.Warn().Str("addr", addr).Msg("relay: plain HTTP on a public bind (--insecure: development only)")
		return nil
	}
	if behindProxy {
		log.Warn().Str("addr", addr).Msg("relay: --behind-proxy on a public bind; the TLS front should reach the relay over loopback or an internal interface")
		return nil
	}
	return fmt.Errorf("relay: refusing to serve plain HTTP on non-loopback bind %s "+
		"(set --tls-cert/--tls-key, --behind-proxy behind a TLS front, or --insecure for development)", addr)
}

// isPublicBind reports whether the bind address reaches beyond loopback
// (same rule as woca-edge's entry-point guard, cmd/woca-edge/main.go:167).
func isPublicBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return true // ":port" binds every interface
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return !ip.IsLoopback()
	}
	return !strings.EqualFold(host, "localhost")
}
