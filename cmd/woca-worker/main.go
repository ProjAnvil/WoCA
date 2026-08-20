// woca-worker is the Phase 3 execution role (design §5.3/§13): it claims
// runs from the shared runstate ledger and executes them through the
// agent, mirroring events into run_events. Deploy alongside woca-server
// against the same database; neither links the interactive surfaces.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/projanvil/woca/internal/worker"
	"github.com/spf13/cobra"
)

func main() {
	var logLevel string
	root := &cobra.Command{
		Use:           "woca-worker",
		Short:         "woca execution worker (claims runs from the shared queue)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)

			// The settings database carries the runstate tables.
			opts := agentbuild.Options{LogLevel: logLevel}
			db, err := openWorkerDB(cmd, &opts)
			if err != nil {
				return err
			}
			defer db.Close()
			ledger, err := runstate.New(db)
			if err != nil {
				return fmt.Errorf("run ledger: %w", err)
			}

			ap, err := agentbuild.BuildAgent(cmd.Context(), opts, log)
			if err != nil {
				return err
			}
			defer ap.PluginMgr.Close()
			defer ap.Agent.Close()

			bus := eventbus.NewLocalWithRetention(1024)
			defer bus.Close()

			w := worker.New(worker.Config{
				Ledger: ledger,
				Agent:  ap.Agent,
				Bus:    bus,
				Logger: log,
				ID:     fmt.Sprintf("worker-%d", os.Getpid()),
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return w.Run(ctx)
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().String("db", "", "settings database: sqlite path, postgres:// DSN (env WOCA_DB)")
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openWorkerDB opens the shared settings/runstate database.
func openWorkerDB(cmd *cobra.Command, opts *agentbuild.Options) (*wocadb.DB, error) {
	target, _ := cmd.Flags().GetString("db")
	if target == "" {
		target = os.Getenv("WOCA_DB")
	}
	if target == "" || target == "none" {
		return nil, fmt.Errorf("the worker requires a shared database (set --db or WOCA_DB)")
	}
	db, err := wocadb.Open(target)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("the worker requires a database")
	}
	return db, nil
}
