// woca-ops is the tenant ops console service. It has exactly two
// assemblies, stated by the operator at process start — never sniffed
// per request:
//
//   - proxy (WOCA_OPS_UPSTREAM, the full/prod topology): a stateless
//     facade — the console SPA plus a reverse proxy onto woca-scheduler's
//     pageless management API. The login exchange and the console
//     session both live server-side; the session cookie transits.
//
//   - direct (WOCA_DB, the single-box dev topology): the console runs
//     self-contained over the shared database — the same authoritative
//     opsconsole API plus the SPA in one process, no woca-scheduler hop.
//
// The upstream address follows the discovery discipline (design §12.1):
// a plain URL or a nacos:// / consul:// reference resolved through the
// same channel as every other service address. The console never
// self-registers.
//
// Both assemblies also serve the skill-asset console (design §6.4) under
// /skillsvc/ (internal/ee/skillsvcui); its /api/skills calls ride the same
// channel as the ops console's API — proxied to woca-scheduler in the proxy
// assembly, in-process over the shared database in the direct one.
package main

import (
	"context"
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
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/opsconsole"
	"github.com/projanvil/woca/internal/ee/opsui"
	"github.com/projanvil/woca/internal/ee/skillsvc"
	"github.com/projanvil/woca/internal/ee/skillsvcui"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/rs/zerolog"
)

func main() {
	// Registry-backed DSN references (WOCA_DB: nacos://…) resolve at open
	// time — same as every server-role entry.
	discovery.Install()
	// Default to info like the original facade logger — the empty string
	// would parse as "disabled" and silence the surface entirely.
	levelStr := os.Getenv("WOCA_LOG_LEVEL")
	if levelStr == "" {
		levelStr = "info"
	}
	level, err := logging.ParseLevel(levelStr)
	if err != nil {
		level = zerolog.InfoLevel
	}
	log := logging.NewConsole(os.Stderr, level)
	addr := os.Getenv("WOCA_OPS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9091"
	}

	// Observability: request spans with the service identity + the
	// healthz/metrics plane on this internal listener.
	obsInst, err := obs.Setup(context.Background(), obs.FromEnv())
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: observability setup failed")
	}
	defer func() { _ = obsInst.Shutdown(context.WithoutCancel(context.Background())) }()
	_ = obsInst.Install(context.Background())

	router := webfx.New(webfx.Options{Logger: log, Addr: addr, Grace: 3 * time.Second})
	router.UseDefaults()
	router.Use(obsInst.HTTPMiddleware())
	router.SetWrap(obsInst.Wrap)
	switch db, upstream := os.Getenv("WOCA_DB"), os.Getenv("WOCA_OPS_UPSTREAM"); {
	case db != "" && upstream != "":
		log.Fatal().Msg("woca-ops: WOCA_DB (direct) and WOCA_OPS_UPSTREAM (proxy) are mutually exclusive — pick one assembly")
	case upstream != "":
		router.Handle("/", proxyAssembly(log, upstream))
	case db != "":
		router.Handle("/", directAssembly(log, db))
	default:
		log.Fatal().Msg("woca-ops: set WOCA_OPS_UPSTREAM (proxy onto woca-scheduler) or WOCA_DB (direct) — refusing to guess an assembly")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := router.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("woca-ops: listener stopped")
	}
}

// proxyAssembly is the full/prod form: the SPA plus a stateless proxy
// onto woca-scheduler's management API.
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
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = upstream.Scheme
			r.Out.URL.Host = upstream.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn().Err(err).Str("path", r.URL.Path).Msg("woca-ops: upstream unreachable")
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/skillsvc/", skillsSPA())
	mux.Handle("/", opsui.Handler())
	log.Info().Str("upstream", resolved).Msg("woca-ops: console facade up (SPA + proxy to woca-scheduler)")
	return mux
}

// skillsSPA serves the skill-asset console (internal/ee/skillsvcui) under
// /skillsvc/; its API calls (/api/skills) ride the same channel as the ops
// console's — proxied to woca-scheduler in the proxy assembly, in-process in
// the direct one.
func skillsSPA() http.Handler {
	return http.StripPrefix("/skillsvc/", skillsvcui.Handler())
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
	skillStore, err := skillsvc.NewStore(st.DB())
	if err != nil {
		log.Fatal().Err(err).Msg("woca-ops: skill asset store failed")
	}
	console := opsconsole.New(st, gwStore, auth, tenantStore, log)
	console.AttachSkills(skillStore)
	mux := http.NewServeMux()
	mux.Handle("/api/", console.Handler())
	mux.Handle("/skillsvc/", skillsSPA())
	mux.Handle("/", opsui.Handler())
	log.Info().Msg("woca-ops: console up (direct over the shared database; no woca-scheduler hop)")
	return mux
}
