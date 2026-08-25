// woca-ops is the tenant ops console service. It has exactly two
// assemblies, stated by the operator at process start — never sniffed
// per request:
//
//   - proxy (WOCA_OPS_UPSTREAM, the full/prod topology): a stateless
//     facade — the console SPA plus a reverse proxy onto woca-server's
//     pageless management API. The login exchange and the console
//     session both live server-side; the session cookie transits.
//
//   - direct (WOCA_DB, the single-box dev topology): the console runs
//     self-contained over the shared database — the same authoritative
//     opsconsole API plus the SPA in one process, no woca-server hop.
//
// The upstream address follows the discovery discipline (design §12.1):
// a plain URL or a nacos:// / consul:// reference resolved through the
// same channel as every other service address. The console never
// self-registers.
package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/opsconsole"
	"github.com/projanvil/woca/internal/ee/opsui"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

func main() {
	// Registry-backed DSN references (WOCA_DB: nacos://…) resolve at open
	// time — same as every server-role entry.
	discovery.Install()
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
	addr := os.Getenv("WOCA_OPS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9091"
	}

	var h http.Handler
	switch db, upstream := os.Getenv("WOCA_DB"), os.Getenv("WOCA_OPS_UPSTREAM"); {
	case db != "" && upstream != "":
		log.Fatal().Msg("woca-ops: WOCA_DB (direct) and WOCA_OPS_UPSTREAM (proxy) are mutually exclusive — pick one assembly")
	case upstream != "":
		h = proxyAssembly(log, upstream)
	case db != "":
		h = directAssembly(log, db)
	default:
		log.Fatal().Msg("woca-ops: set WOCA_OPS_UPSTREAM (proxy onto woca-server) or WOCA_DB (direct) — refusing to guess an assembly")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", addr).Msg("woca-ops: listen failed")
	}
	hs := &http.Server{Addr: addr, Handler: h}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("woca-ops: listener stopped")
	}
}

// proxyAssembly is the full/prod form: the SPA plus a stateless proxy
// onto woca-server's management API.
func proxyAssembly(log zerolog.Logger, raw string) http.Handler {
	resolved, err := discovery.Resolve(context.Background(), raw)
	if err != nil {
		log.Fatal().Err(err).Str("upstream", raw).Msg("woca-ops: upstream discovery failed")
	}
	upstream, err := url.Parse(resolved)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		log.Fatal().Str("upstream", resolved).Msg("woca-ops: WOCA_OPS_UPSTREAM must be an http(s) URL")
	}
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = upstream.Scheme
			r.URL.Host = upstream.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn().Err(err).Str("path", r.URL.Path).Msg("woca-ops: upstream unreachable")
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/", opsui.Handler())
	log.Info().Str("upstream", resolved).Msg("woca-ops: console facade up (SPA + proxy to woca-server)")
	return mux
}

// directAssembly is the single-box dev form: the authoritative opsconsole
// API over the shared database plus the SPA, one process, no server hop.
func directAssembly(log zerolog.Logger, dsn string) http.Handler {
	st, err := settings.Open(dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: settings store open failed")
	}
	gwStore, err := entrygw.NewStore(st.DB())
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: gateway registry store failed")
	}
	tenantStore, err := tenant.NewStore(st.DB())
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: tenant store failed")
	}
	auth, err := authcore.New(tenantStore, st.DB(), log)
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: core auth service failed")
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", opsconsole.New(st, gwStore, auth, tenantStore, log).Handler())
	mux.Handle("/", opsui.Handler())
	log.Info().Msg("woca-ops: console up (direct over the shared database; no woca-server hop)")
	return mux
}
