// woca-server is the server-edition entry point: the modular-monolith
// server surface (serve + web daemon) without the interactive surfaces
// (TUI/REPL/one-shot CLI). Deployed behind the enterprise gateway; the
// personal `woca` binary keeps everything (design §5.1).
package main

import (
	"fmt"
	"os"

	"github.com/projanvil/woca/internal/agentbuild"
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
	root.AddCommand(servefront.NewServeCmd(opts))
	root.AddCommand(servefront.NewWebCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
