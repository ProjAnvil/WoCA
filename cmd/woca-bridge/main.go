// woca-bridge is the private-cloud bridge service (product design v1.7):
// the execution plane users hold themselves. It is the enterprise-edition
// server role for user machines — the same serve path as woca-server,
// with the product identity model layered on top: organization
// credentials (issued by the tenant ops console on the private core,
// verified offline against the config-delivered org key), cookie/Bearer
// login sessions with sliding expiry, and the fail-closed bind check
// (R1). The bridge carries NO key management; an unenrolled bridge keeps
// only loopback trust.
//
// Cloud access (design §6.5, v1.5): the channel to the edge access point
// — enrollment, managed-config pulls, heartbeats, and the scenario-four
// tunnel that lets any-place web command this bridge through
// https://edge.example.com — is bridge-local configuration, editable on
// the settings page (云接入) or preset with WOCA_EDGE_URL /
// WOCA_ENROLL_TOKEN.
//
// The serve-front edition wiring itself lives in internal/ee/bridge
// (ServeState.Edition) so test harnesses assemble the identical bridge.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/bridge"
	"github.com/projanvil/woca/internal/servefront"
)

func main() {
	opts := &agentbuild.Options{}
	root := &cobra.Command{
		Use:           "woca-bridge",
		Short:         "woca bridge (private-cloud execution plane)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)
	if f := root.PersistentFlags().Lookup("log-level"); f != nil {
		f.DefValue = "info"
		_ = f.Value.Set("info")
	}

	state := &bridge.ServeState{}
	serve := servefront.NewServeCmdWithEdition(opts, state.Edition())
	// Fail-closed red line R1: refuse non-loopback binds without
	// authentication before anything starts.
	origRun := serve.RunE
	serve.RunE = func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		state.AuthMode.Store("local")
		if mode, _ := cmd.Flags().GetString("auth"); mode != "" {
			state.AuthMode.Store(mode)
		} else if mode := os.Getenv("WOCA_AUTH"); mode != "" {
			state.AuthMode.Store(mode)
		}
		if err := bridge.CheckBind(addr, state.TunnelMode()); err != nil {
			return err
		}
		return origRun(cmd, args)
	}
	root.AddCommand(serve)
	root.AddCommand(newGatewayCmd())
	root.AddCommand(servefront.NewWebCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
