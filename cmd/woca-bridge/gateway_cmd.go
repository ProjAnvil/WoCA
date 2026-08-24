package main

// The bridge's gateway channel commands: manual enrollment with a
// one-time token from the control-plane console (product design §2.3).
// After enrollment `woca-bridge serve` resumes the channel automatically.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/bridge"
	"github.com/spf13/cobra"
)

func newGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Manage the entry-gateway channel (enroll, status)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "enroll",
		Short: "Register this bridge with the entry gateway (one-time token)",
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			if url == "" {
				url = os.Getenv("WOCA_GATEWAY_URL")
			}
			if token == "" {
				token = os.Getenv("WOCA_ENROLL_TOKEN")
			}
			if url == "" || token == "" {
				return fmt.Errorf("gateway enroll requires --url and --token (or WOCA_GATEWAY_URL / WOCA_ENROLL_TOKEN)")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			insecure, _ := cmd.Flags().GetBool("insecure")
			g := bridge.NewGatewayClient(bridge.GatewayConfig{
				URL: url, EnrollmentToken: token,
				StatePath: filepath.Join(home, ".woca", "bridge-gateway.json"),
				Insecure:  insecure,
			}, zerolog.Nop())
			if err := g.Enroll(cmd.Context()); err != nil {
				return err
			}
			if insecure {
				fmt.Println("warning: certificate verification disabled (development mode)")
			}
			fmt.Printf("enrolled: %s\n", g.Status().BridgeID)
			fmt.Println("the enrollment token is spent; `woca-bridge serve` now pulls config through the gateway")
			return nil
		},
	})
	enroll := cmd.Commands()[0]
	enroll.Flags().String("url", "", "entry-gateway base URL")
	enroll.Flags().String("token", "", "one-time enrollment token")
	enroll.Flags().Bool("insecure", false, "skip gateway certificate verification (development 联调)")

	// Worker-led flow: no token pasting — the console approves the
	// machine's key fingerprint (product design §2.3).
	req := &cobra.Command{
		Use:   "request",
		Short: "Register this machine and wait for console approval (worker-led)",
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			if url == "" {
				url = os.Getenv("WOCA_GATEWAY_URL")
			}
			if url == "" {
				return fmt.Errorf("gateway request requires --url (or WOCA_GATEWAY_URL)")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			insecure, _ := cmd.Flags().GetBool("insecure")
			g := bridge.NewGatewayClient(bridge.GatewayConfig{
				URL: url, StatePath: filepath.Join(home, ".woca", "bridge-gateway.json"),
				Insecure: insecure,
			}, consoleLogger())
			return g.RequestEnrollment(cmd.Context(), func(fp string) {
				fmt.Printf("enrollment fingerprint: %s\n", fp)
				fmt.Println("ask the control-plane administrator to approve it (console → pending enrollments); waiting…")
			})
		},
	}
	req.Flags().String("url", "", "entry-gateway base URL")
	req.Flags().Bool("insecure", false, "skip gateway certificate verification (development 联调)")
	cmd.AddCommand(req)
	return cmd
}

// consoleLogger prints channel progress to stderr.
func consoleLogger() zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
}
