// woca-server is the server-edition entry point: the modular-monolith
// server surface (serve + web daemon) without the interactive surfaces
// (TUI/REPL/one-shot CLI). Deployed behind the enterprise gateway; the
// personal `woca` binary keeps everything (design §5.1).
//
// Server governance baseline (design §12.3): OTel SDK embedded, /healthz
// and /metrics on the business listener, RED metrics per route, spans
// exported over OTLP when WOCA_OTEL_EXPORTER_OTLP_ENDPOINT is set.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/obs"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/spf13/cobra"
)

func main() {
	opts := &agentbuild.Options{}
	root := &cobra.Command{
		Use:           "woca-server",
		Short:         "woca server edition (no interactive surfaces)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)
	// The agentbuild default (warn) keeps the personal terminal quiet; a
	// server role must not run silent — its config watcher and message-plane
	// lifelines are operational signals. Server editions default to info.
	if f := root.PersistentFlags().Lookup("log-level"); f != nil {
		f.DefValue = "info"
		_ = f.Value.Set("info")
	}
	root.AddCommand(servefront.NewServeCmdWithObs(opts, func(ctx context.Context) (servefront.Observability, error) {
		return obs.Setup(ctx, obs.FromEnv())
	}))
	root.AddCommand(servefront.NewWebCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
