// woca-server is the enterprise-edition server role: the modular-monolith
// API surface (business API + the console management authority) with no
// interactive surfaces (TUI/REPL/one-shot CLI) and no web pages — the
// chat UI ships with the execution planes (personal `woca`, bridge) and
// the console UI with the separate woca-ops facade. Deployed behind the
// enterprise gateway; the personal `woca` binary keeps everything
// (design §5.1).
//
// Server governance baseline (design §12.3): OTel SDK embedded, /healthz
// and /metrics on the business listener, RED metrics per route, spans
// exported over OTLP when WOCA_OTEL_EXPORTER_OTLP_ENDPOINT is set.
//
// Edition wiring: this entry point attaches the enterprise assembly
// (observability, API-key + native-client auth, message plane) and
// installs the registry DSN resolver — everything the shared serve path
// leaves to the entry point.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/edition"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/opsconsole"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func main() {
	discovery.Install()
	opts := &agentbuild.Options{}
	root := &cobra.Command{
		Use:           "woca-server",
		Short:         "woca enterprise server role (no interactive surfaces)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)
	// The agentbuild default (warn) keeps the personal terminal quiet; a
	// server role must not run silent — its config watcher and message-plane
	// lifelines are operational signals. Server roles default to info.
	if f := root.PersistentFlags().Lookup("log-level"); f != nil {
		f.DefValue = "info"
		_ = f.Value.Set("info")
	}
	root.AddCommand(servefront.NewServeCmdWithEdition(opts, servefront.Edition{
		// woca-server is a pageless API role: it serves the business API
		// and the management authority, but no web pages. The chat UI
		// ships with the execution planes (personal woca, bridge); the
		// ops console UI is the woca-ops facade proxying back to the
		// management listener below.
		Static: nil,
		// The native-client credential surface (edition.Authn mounts it):
		// the activation exchange is anonymous; the whole /api/native/
		// prefix is the client's own session lifecycle, not a host
		// surface, so it passes the local-tenant gate.
		AnonymousAPIPaths:      []string{"/api/native/activate"},
		HostGateExemptPrefixes: []string{"/api/native/"},
		Obs: func(ctx context.Context) (servefront.Observability, error) {
			return obs.Setup(ctx, obs.FromEnv())
		},
		// Full-stack registry assembly: register with nacos and take
		// configuration from the delivered pack. WOCA_NACOS_URL is
		// required — without the registry the process refuses to start
		// (fail-closed, no fallback posture).
		Nacos: func() *servefront.NacosAssembly {
			base := nacosrt.ServerFromEnv()
			return &servefront.NacosAssembly{
				Server:  base,
				Service: "woca-server",
				Build: func(a servefront.NacosAssembly, w servefront.NacosWiring, log zerolog.Logger) (servefront.NacosRuntime, error) {
					return nacosrt.New(nacosrt.Config{
						Server: a.Server, Service: a.Service, Advertise: w.Advertise,
						St: w.St, DB: w.DB, Identity: w.Identity, OnChange: w.OnChange,
					}, log)
				},
			}
		}(),
		Authn: edition.Authn,
		Plane: edition.Plane,
		// Service-plane gateway listener (product design v1.5 拓扑拆分):
		// the machine registry the edge access point calls home to —
		// enrollment, renewal, heartbeats, /gw/resolve, /gw/bridges — plus
		// the console management API (v1.7 §6.7): tenants, woca_ keys,
		// sessions, enrollments, audit — the authoritative, pageless
		// surface. The console UI is the separate woca-ops facade (SPA +
		// proxy onto this listener). Bind it on the internal network only;
		// the public entry is woca-edge, never this listener, and the edge
		// forwards /gw/* machine routes exclusively — the console is never
		// exposed through it.
		Start: func(ctx context.Context, st *settings.Store, log zerolog.Logger) {
			addr := os.Getenv("WOCA_GW_ADDR")
			if addr == "" || st == nil {
				return
			}
			gwStore, err := entrygw.NewStore(st.DB())
			if err != nil {
				log.Error().Err(err).Msg("woca-server: gateway registry store failed")
				return
			}
			// The user-identity authority (v1.8 §2.1): woca_ key login,
			// opaque session issuance/introspection/revocation. The edge
			// calls it home as the auth enforcement point; direct-connect
			// bridges reach it through the edge's /gw forwarding.
			tenantStore, err := tenant.NewStore(st.DB())
			if err != nil {
				log.Error().Err(err).Msg("woca-server: tenant store failed")
				return
			}
			auth, err := authcore.New(tenantStore, st.DB(), log)
			if err != nil {
				log.Error().Err(err).Msg("woca-server: core auth service failed")
				return
			}
			audit := func(event, subject, detail string) { gwStore.AuditEvent(context.Background(), event, subject, detail) }
			// entrygw's route registrar is mux-typed, so the machine
			// channel mounts on a stdlib mux delegated as a subtree
			// (paths stay absolute); everything else is webfx.
			gwMux := http.NewServeMux()
			entrygw.NewRegistryHandler(gwStore).RegisterRoutes(gwMux)
			gwMux.Handle("/gw/auth/", authcore.Handler(auth, audit))
			console := opsconsole.New(st, gwStore, auth, tenantStore, log)
			// Gateway-policy writes ride the config pack in the
			// full-stack posture (nil = the direct dev console, which
			// shares the database with its servers).
			if base := nacosrt.ServerFromEnv(); base != "" {
				console.AttachPublisher(nacosrt.NewClient(base))
			}
			router := webfx.New(webfx.Options{Logger: log, Grace: 3 * time.Second})
			router.UseDefaults()
			router.Handle("/gw/", gwMux)
			router.Handle("/", console.Handler())
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				log.Error().Err(err).Str("addr", addr).Msg("woca-server: gateway listener failed")
				return
			}
			go func() {
				if err := router.Serve(ctx, ln); err != nil {
					log.Error().Err(err).Msg("woca-server: gateway listener stopped")
				}
			}()
			log.Info().Str("addr", addr).Msg("woca-server: service-plane gateway listener up (/gw/* + console management API; UI on woca-ops)")
		},
	}))
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
