// executor-proxy is the EE sandbox plane's stateless router (spec §3.2):
// it stands in front of the sandbox executors, allocating-or-binding
// workspaces (POST /internal/bindings) and forwarding every sandbox
// request to the executor its workspace is bound to. The proxy holds no
// process state worth registering: it does NOT register with nacos and
// writes NO ack of its own — its whole world is the shared woca database
// (workspace bindings + executor config acks), resolved through WOCA_DB
// (nacos:// references included, via the discovery DSN seam).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/discovery"
	"github.com/projanvil/woca/internal/ee/proxy"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/spf13/cobra"
)

// listenEnv overrides the default listener address.
const listenEnv = "WOCA_PROXY_LISTEN"

func main() {
	discovery.Install()
	root := newProxyRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newProxyRoot() *cobra.Command {
	var logLevel string
	root := &cobra.Command{
		Use:           "executor-proxy",
		Short:         "woca stateless sandbox workspace router (bindings + forwarding)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)

			// The shared database is the proxy's only dependency: bindings
			// in, executor acks out. Registry references (nacos://) resolve
			// through the seam discovery.Install armed above.
			target, _ := cmd.Flags().GetString("db")
			if target == "" {
				target = os.Getenv("WOCA_DB")
			}
			if target == "" || target == "none" {
				return fmt.Errorf("executor-proxy requires a shared database (set --db or WOCA_DB)")
			}
			db, err := wocadb.Open(target)
			if err != nil {
				return err
			}
			if db == nil {
				return fmt.Errorf("executor-proxy requires a database")
			}
			defer func() { _ = db.Close() }()

			p := proxy.New(proxy.Config{DB: db, Logger: log})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go p.Run(ctx) // degraded-binding scan; stateless otherwise

			listen, _ := cmd.Flags().GetString("listen")
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("proxy listener %s: %w", listen, err)
			}
			srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = srv.Serve(ln) }()
			fmt.Fprintf(os.Stderr, "executor-proxy: %s\n", listen)

			<-ctx.Done()
			log.Info().Msg("executor-proxy: shutting down")
			shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().String("listen", defaultListen(), "sandbox routing listener (env WOCA_PROXY_LISTEN)")
	root.Flags().String("db", "", "shared woca database: sqlite path, postgres:// or nacos:// DSN (env WOCA_DB)")
	return root
}

// defaultListen resolves the listener default from the environment
// (:8094 — the server-plane 809x block after gw :8090 and ops :8091; the
// executor contract service takes :8093).
func defaultListen() string {
	if v := os.Getenv(listenEnv); v != "" {
		return v
	}
	return ":8094"
}
