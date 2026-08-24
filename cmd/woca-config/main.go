// woca-config is the standalone settings management plane (design §16):
// the sole configuration write surface for server deployments, serving the
// settings API (read/write/export/import) and the embedded ops console
// SPA. Deployed behind the gateway's admin port. The console signs in
// with a username/password when WOCA_CONSOLE_USER/WOCA_CONSOLE_PASSWORD
// are set at startup; unset credentials leave it open (the operator
// declared the network the boundary).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/configsvc"
	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/msgplane"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/telemetry"
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
			obsInst.Install(cmd.Context())

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
			defer st.Close()
			ts, err := tenant.NewStore(st.DB())
			if err != nil {
				return fmt.Errorf("tenant store: %w", err)
			}

			// Message-plane publisher (design §16.7): the resolver reads the
			// store live, so plane-mode changes apply to the next write's
			// notification without restarting this service.
			plane := msgplane.New(func() msgplane.Config {
				c := st.Load()
				return msgplane.Config{Mode: c.MsgPlaneMode, NATSURL: c.MsgPlaneNATSURL, Subject: c.MsgPlaneSubject}
			}, log)
			defer plane.Close()

			svc, err := configsvc.New(st, ts, st.DB(), plane, log)
			if err != nil {
				return err
			}
			handler := svc.Handler()
			if obsAddr != "" && obsAddr != "off" {
				handler = obsInst.Wrap(handler)
			}
			hs := &http.Server{Addr: addr, Handler: handler}
			serveErr := make(chan error, 1)
			go func() { serveErr <- hs.ListenAndServe() }()
			fmt.Fprintf(os.Stderr, "woca-config: %s\n", addr)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			select {
			case err := <-serveErr:
				if err == http.ErrServerClosed {
					return nil
				}
				return err
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return hs.Shutdown(shutdownCtx)
			}
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().StringVar(&addr, "addr", ":8080", "listen address (env WOCA_CONFIG_ADDR)")
	root.Flags().StringVar(&obsAddr, "obs-addr", defaultObsAddr(), "healthz/metrics listener address, or \"off\" (env WOCA_OBS_ADDR)")
	root.Flags().String("db", "", "shared settings database: sqlite path, postgres:// DSN (env WOCA_DB)")
	if env := os.Getenv("WOCA_CONFIG_ADDR"); env != "" {
		root.Flags().Set("addr", env)
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
