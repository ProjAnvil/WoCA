package main

import (
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/ee/memcmd"
	"github.com/projanvil/woca/internal/ee/tenantcmd"
	"github.com/spf13/cobra"
)

// isolateEnv points HOME and WOCA_DB at a temp directory so tests never
// touch the developer's real ~/.woca state.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WOCA_DB", filepath.Join(home, "woca.db"))
	return home
}

// execCmd runs the woca-tenant root command in-process.
func execCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := &cobra.Command{Use: "woca-tenant", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(tenantcmd.New())
	root.AddCommand(memcmd.New())
	root.SetArgs(args)
	return root.Execute()
}
