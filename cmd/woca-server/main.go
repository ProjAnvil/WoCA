// woca-server is the enterprise-edition server role: the modular-monolith
// server surface (serve + web daemon) without the interactive surfaces
// (TUI/REPL/one-shot CLI). Deployed behind the enterprise gateway; the
// personal `woca` binary keeps everything (design §5.1).
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
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/opsconsole"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/settings"
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
		// The native-client credential surface (edition.Authn mounts it):
		// the activation exchange is anonymous; the whole /api/native/
		// prefix is the client's own session lifecycle, not a host
		// surface, so it passes the local-tenant gate.
		AnonymousAPIPaths:      []string{"/api/native/activate"},
		HostGateExemptPrefixes: []string{"/api/native/"},
		Obs: func(ctx context.Context) (servefront.Observability, error) {
			return obs.Setup(ctx, obs.FromEnv())
		},
		Authn: edition.Authn,
		Plane: edition.Plane,
		// Service-plane gateway listener (product design v1.5 拓扑拆分):
		// the machine registry the edge access point calls home to —
		// enrollment, renewal, heartbeats, /gw/resolve, /gw/bridges — plus
		// the tenant ops console (v1.7 §6.7): the private core's single
		// management surface (org credentials, enrollments, audit). Bind it
		// on the internal network only; the public entry is woca-edge,
		// never this listener, and the edge forwards /gw/* machine routes
		// exclusively — the console is never exposed through it.
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
			mux := http.NewServeMux()
			entrygw.NewRegistryHandler(gwStore).RegisterRoutes(mux)
			mux.Handle("/gw/auth/", authcore.Handler(auth, audit))
			mux.Handle("/", opsconsole.New(st, gwStore, auth, tenantStore, log).Handler())
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				log.Error().Err(err).Str("addr", addr).Msg("woca-server: gateway listener failed")
				return
			}
			hs := &http.Server{Addr: addr, Handler: mux}
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = hs.Shutdown(shutdownCtx)
			}()
			go func() {
				if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Error().Err(err).Msg("woca-server: gateway listener stopped")
				}
			}()
			log.Info().Str("addr", addr).Msg("woca-server: service-plane gateway listener up (/gw/* + ops console)")
		},
	}))
	root.AddCommand(servefront.NewWebCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
