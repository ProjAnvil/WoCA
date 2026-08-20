package main

import (
	"fmt"
	"os"

	"github.com/projanvil/woca/internal/memory"
	"github.com/spf13/cobra"
)

// newMemoryMigrateCmd builds `woca memory export|import`: the Phase 2
// personal→enterprise memory migration path (design §6.3.8). Export reads
// the embedded personal engine; import writes through the remote ladyM
// enterprise client.
func newMemoryMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Migrate factual memories between engines (personal ↔ enterprise)",
	}

	export := &cobra.Command{
		Use:   "export",
		Short: "Export workspaces of the embedded personal engine as JSONL",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath, _ := cmd.Flags().GetString("db")
			outPath, _ := cmd.Flags().GetString("out")
			workspaces, _ := cmd.Flags().GetStringArray("workspace")
			if len(workspaces) == 0 {
				return fmt.Errorf("export requires at least one --workspace")
			}
			eng, err := memory.Build(memory.BuildInput{
				EngineEnabled:    true,
				DBPath:           dbPath,
				DefaultWorkspace: workspaces[0],
				Embedding:        "hashing",
				LLM:              "none",
			})
			if err != nil {
				return err
			}
			defer eng.Close()

			w := os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			n, err := memory.ExportEngine(cmd.Context(), eng, workspaces, w)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "exported %d memories\n", n)
			return nil
		},
	}
	export.Flags().String("db", "", "personal ladyM sqlite path (default ~/.woca/ladym.db)")
	export.Flags().String("out", "", "output file (default stdout)")
	export.Flags().StringArray("workspace", nil, "workspace to export (repeatable; the woca workspace path)")

	importCmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import JSONL memories into a remote ladyM enterprise engine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint, _ := cmd.Flags().GetString("endpoint")
			username, _ := cmd.Flags().GetString("username")
			password, _ := cmd.Flags().GetString("password")
			target, _ := cmd.Flags().GetString("workspace")
			eng, err := memory.BuildRemote(memory.RemoteConfig{
				Endpoint: endpoint, Username: username, Password: password,
			})
			if err != nil {
				return err
			}
			defer eng.Close()

			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			imported, dropped, err := memory.ImportInto(cmd.Context(), eng, f, target)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "imported %d memories (%d dropped by the attention gate)\n", imported, dropped)
			return nil
		},
	}
	importCmd.Flags().String("endpoint", "", "ladyM enterprise API endpoint (required)")
	importCmd.Flags().String("username", "", "ladyM API username (Basic auth; empty for auth=off deployments)")
	importCmd.Flags().String("password", "", "ladyM API password")
	importCmd.Flags().String("workspace", "", "target workspace key (a woca:… key passes through unchanged)")
	_ = importCmd.MarkFlagRequired("endpoint")
	_ = importCmd.MarkFlagRequired("workspace")

	cmd.AddCommand(export, importCmd)
	return cmd
}
