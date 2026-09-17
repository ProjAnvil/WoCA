package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/projanvil/woca/internal/relay"
	"github.com/spf13/cobra"
)

// newKeyCmd builds `woca-relay key create|list|revoke` (design §4.3).
func newKeyCmd(dbPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage rendezvous keys (create | list | revoke)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "create <name>",
		Short: "Mint a new wcr_ key (printed once — store it now)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			k, raw, err := ks.CreateKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			// The full key goes to stdout exactly once (scripts scrape it);
			// the store keeps only its SHA-256 (design §4.1).
			fmt.Printf("%s\n", raw)
			fmt.Fprintf(os.Stderr, "created key %q — the full key above is shown only once\n", k.Name)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List keys (prefix, created, last used, status, bridge online)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			keys, err := ks.ListKeys(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPREFIX\tCREATED\tLAST USED\tSTATUS\tBRIDGE")
			for _, k := range keys {
				status := "active"
				if k.RevokedAt != nil {
					status = "revoked"
				}
				lastUsed := "-"
				bridge := "offline"
				if k.LastUsedAt != nil {
					lastUsed = k.LastUsedAt.Format(time.DateTime)
					// Bridge online = tunnel activity within the generation
					// lease window (design §6.2).
					if time.Since(*k.LastUsedAt) < relay.GenerationStaleAfter {
						bridge = "online"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					k.Name, k.Prefix, k.CreatedAt.Format(time.DateTime), lastUsed, status, bridge)
			}
			return w.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a key (the running relay cascades it to both sides)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ks, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = ks.Close() }()
			k, ok, err := ks.RevokeKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("unknown or already revoked key %q", args[0])
			}
			// The revocation itself is this row write; the running relay's
			// sweep (serve subcommand) notices it and cancels in-flight
			// polls, deletes sessions, and drains the parking queue.
			fmt.Printf("revoked key %q (%s)\n", k.Name, k.Prefix)
			return nil
		},
	})
	return cmd
}
