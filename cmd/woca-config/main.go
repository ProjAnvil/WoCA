// woca-config is the standalone settings management plane (design §16):
// the sole configuration write surface for server deployments, serving the
// settings API (read/write/export/import) and its own console SPA (built
// from ee/configsvc-web; the tenant ops console SPA is internal/ee/opsui,
// served by woca-ops). Deployed behind the gateway's admin port. The
// console signs in
// with a username/password when WOCA_CONSOLE_USER/WOCA_CONSOLE_PASSWORD
// are set at startup; unset credentials leave it open (the operator
// declared the network the boundary).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/configsvc"
	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/webfx"
	"github.com/spf13/cobra"
)

func main() {
	discovery.Install()
	var logLevel, addr, obsAddr string
	root := &cobra.Command{
		Use:           "woca-config",
		Short:         "woca configuration management plane (settings API + console)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			telemetry.SetDefault(telemetry.NewLocalSink(log))
			obsInst, err := obs.Setup(cmd.Context(), obs.FromEnv())
			if err != nil {
				return fmt.Errorf("observability: %w", err)
			}
			defer func() { _ = obsInst.Shutdown(context.WithoutCancel(cmd.Context())) }()
			_ = obsInst.Install(cmd.Context())

			target, _ := cmd.Flags().GetString("db")
			if target == "" {
				target = os.Getenv("WOCA_DB")
			}
			if target == "" || target == "none" {
				return fmt.Errorf("the config plane requires the shared database (set --db or WOCA_DB)")
			}
			st, err := settings.Open(target)
			if err != nil {
				return fmt.Errorf("settings store: %w", err)
			}
			defer func() { _ = st.Close() }()
			ts, err := tenant.NewStore(st.DB())
			if err != nil {
				return fmt.Errorf("tenant store: %w", err)
			}

			svc, err := configsvc.New(st, ts, st.DB(), log)
			if err != nil {
				return err
			}
			// Delivery channel (full-stack posture): every save publishes
			// the pack to nacos — the sole source of truth. WOCA_NACOS_URL
			// is required; a config plane that cannot reach its registry
			// refuses to start.
			nacosBase := nacosrt.ServerFromEnv()
			if nacosBase == "" {
				return fmt.Errorf("config plane requires WOCA_NACOS_URL (the nacos registry); refusing to start without it")
			}
			if err := svc.AttachNacos(cmd.Context(), nacosBase); err != nil {
				return err
			}
			// The webfx router owns the surface: request id + request log +
			// recovery around the config plane's handler, with the obs
			// probe plane (healthz/metrics) at the outermost edge when
			// enabled. Shutdown rides the router's 3s grace.
			router := webfx.New(webfx.Options{
				Logger: log,
				Addr:   addr,
				Grace:  3 * time.Second,
			})
			router.UseDefaults()
			// Request spans: every config-plane request lands in the
			// collector attributed to the service's registry identity —
			// the HTTP counterpart of the chat/tool spans.
			router.Use(obsInst.HTTPMiddleware())
			router.Handle("/", svc.Handler())
			if obsAddr != "" && obsAddr != "off" {
				router.SetWrap(obsInst.Wrap)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(os.Stderr, "woca-config: %s\n", addr)
			return router.Run(ctx)
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().StringVar(&addr, "addr", ":8080", "listen address (env WOCA_CONFIG_ADDR)")
	root.Flags().StringVar(&obsAddr, "obs-addr", defaultObsAddr(), "healthz/metrics listener address, or \"off\" (env WOCA_OBS_ADDR)")
	root.Flags().String("db", "", "shared settings database: sqlite path, postgres:// DSN (env WOCA_DB)")
	if env := os.Getenv("WOCA_CONFIG_ADDR"); env != "" {
		_ = root.Flags().Set("addr", env)
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func defaultObsAddr() string {
	if v := os.Getenv("WOCA_OBS_ADDR"); v != "" {
		return v
	}
	return ":9093"
}
