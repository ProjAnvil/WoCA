package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/tenant"
	"github.com/projanvil/woca/internal/wocadb"
)

// newTenantCmd builds `woca tenant …`: manage tenants and API keys on the
// settings database (sqlite path or postgres DSN, resolved from --db /
// WOCA_DB like every woca command). Keys are shown exactly once at issue
// time; only their SHA-256 hash is stored.
func newTenantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Manage tenants and API keys (platform edition)",
	}
	cmd.PersistentFlags().String("db", "", "settings database: sqlite path, postgres:// DSN (env WOCA_DB)")

	open := func(cmd *cobra.Command) (*tenant.Store, func(), error) {
		target, _ := cmd.Flags().GetString("db")
		if target == "" {
			target = os.Getenv("WOCA_DB")
		}
		db, err := wocadb.Open(target)
		if err != nil {
			return nil, nil, err
		}
		if db == nil {
			return nil, nil, fmt.Errorf("tenant management requires a database (set --db; it cannot be \"none\")")
		}
		st, err := tenant.NewStore(db)
		if err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		return st, func() { _ = db.Close() }, nil
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "create <name>",
		Short: "Create a tenant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeDB, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeDB()
			t, err := st.CreateTenant(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("tenant created: %s (%s)\n", t.ID, t.Name)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List tenants",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeDB, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeDB()
			ts, err := st.ListTenants(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, t := range ts {
				fmt.Fprintf(w, "%s\t%s\n", t.ID, t.Name)
			}
			return w.Flush()
		},
	})

	issue := &cobra.Command{
		Use:   "issue <tenant-id>",
		Short: "Issue an API key (the secret is printed once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, _ := cmd.Flags().GetString("role")
			user, _ := cmd.Flags().GetString("user")
			name, _ := cmd.Flags().GetString("name")
			st, closeDB, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeDB()
			ctx := cmd.Context()
			if user == "" {
				label := name
				if label == "" {
					label = "key"
				}
				u, err := st.CreateUser(ctx, args[0], label, role)
				if err != nil {
					return err
				}
				user = u.ID
			}
			k, full, err := st.IssueKey(ctx, args[0], user, name, role)
			if err != nil {
				return err
			}
			fmt.Printf("API key issued (tenant %s, role %s)\n", k.TenantID, k.Role)
			fmt.Println("Store it now — it is shown exactly once:")
			fmt.Println(full)
			return nil
		},
	}
	issue.Flags().String("role", authz.RoleMember, "key role: "+authz.RoleAdmin+", "+authz.RoleMember+", or "+authz.RoleViewer)
	issue.Flags().String("user", "", "user id (default: create a user named after --name)")
	issue.Flags().String("name", "", "key label")
	cmd.AddCommand(issue)

	cmd.AddCommand(&cobra.Command{
		Use:   "keys <tenant-id>",
		Short: "List a tenant's API keys (prefixes only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeDB, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeDB()
			ks, err := st.ListKeys(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PREFIX\tROLE\tNAME\tREVOKED")
			for _, k := range ks {
				revoked := "-"
				if k.RevokedAt != 0 {
					revoked = strconv.FormatInt(k.RevokedAt, 10)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.Prefix, k.Role, k.Name, revoked)
			}
			return w.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <prefix>",
		Short: "Revoke an API key by prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, closeDB, err := open(cmd)
			if err != nil {
				return err
			}
			defer closeDB()
			if err := st.RevokeKey(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Println("revoked")
			return nil
		},
	})

	return cmd
}
