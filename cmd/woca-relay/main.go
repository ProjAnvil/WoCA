// woca-relay is the personal-edition remote relay (design
// 2026-09-17-personal-relay-design): a single public-entry binary for a
// VPS. NAT-hidden personal woca instances poll it outbound (bridge side);
// browsers and native clients reach it with a wcr_ rendezvous key (client
// side). It holds no business data — keys, sessions, and an audit log
// only; runs, conversations, and files stay on the bridge.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/projanvil/woca/internal/relay"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var dbPath string
	root := &cobra.Command{
		Use:           "woca-relay",
		Short:         "woca personal remote relay (public entry + key authority)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "relay database path (env WOCA_RELAY_DB)")
	root.AddCommand(newKeyCmd(&dbPath))
	root.AddCommand(newServeCmd(&dbPath))
	return root
}

// defaultDBPath is the operator-conventional relay database location.
func defaultDBPath() string {
	if v := os.Getenv("WOCA_RELAY_DB"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "woca-relay.db"
	}
	return filepath.Join(home, ".woca-relay", "relay.db")
}

// openKeys opens the key/session store for one CLI invocation.
func openKeys(dbPath string) (*relay.KeyStore, error) {
	return relay.OpenKeyStore(dbPath)
}
