// woca-tenant is the tenant administration entry point: tenant/user/API-key
// lifecycle plus the personal→enterprise memory migration tool. Server-side
// only — no interactive surfaces (design §5.1).
package main

import (
	"fmt"
	"os"

	"github.com/projanvil/woca/internal/memcmd"
	"github.com/projanvil/woca/internal/tenantcmd"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "woca-tenant",
		Short:         "woca tenant administration",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(tenantcmd.New())
	root.AddCommand(memcmd.New())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
