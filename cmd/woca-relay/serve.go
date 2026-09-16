package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/relay"
	"github.com/projanvil/woca/internal/webfx"
	webdist "github.com/projanvil/woca/web"
	"github.com/spf13/cobra"
)

// newServeCmd builds `woca-relay serve`: the public listener (client SPA
// + auth + tunnel) with the fail-closed bind policy (design §7.2/§7.3).
func newServeCmd(dbPath *string) *cobra.Command {
	var logLevel, addr, tlsCert, tlsKey string
	var insecure, behindProxy bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the public relay listener (SPA + auth + tunnel)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			keys, err := openKeys(*dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = keys.Close() }()

			tlsOn := tlsCert != "" && tlsKey != ""
			if err := relay.CheckBind(addr, tlsOn, insecure, behindProxy, log); err != nil {
				return err
			}
			rl := relay.NewRelay(keys, webdist.DistFS(), behindProxy, log)

			// Revocation plane (design §5): the audit trail plus the sweep
			// that turns CLI-issued revokes into the live cascade.
			auditLog, err := relay.NewAuditLog(*dbPath + ".audit.log")
			if err != nil {
				return err
			}
			defer func() { _ = auditLog.Close() }()
			revoker := rl.AttachRevocation(auditLog)

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			scheme := "http"
			if tlsOn {
				cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
				if err != nil {
					_ = ln.Close()
					return fmt.Errorf("tls: load keypair: %w", err)
				}
				ln = tls.NewListener(ln, &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{cert},
				})
				scheme = "https"
			}
			// webfx owns the surface: request id + request log + recovery.
			// The 40s grace matches the edge posture: parked polls wait up
			// to 25s and SSE relays stream as long as the bridge answers.
			router := webfx.New(webfx.Options{
				Logger:            log,
				Grace:             40 * time.Second,
				ReadHeaderTimeout: 15 * time.Second,
			})
			router.UseDefaults()
			router.Handle("/", rl.Handler())

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go revoker.Watch(ctx, time.Second)
			fmt.Fprintf(os.Stderr, "woca-relay: %s://%s\n", scheme, ln.Addr())
			return router.Serve(ctx, ln)
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	cmd.Flags().StringVar(&addr, "addr", ":443", "public listen address (env WOCA_RELAY_ADDR)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate for the public listener (env WOCA_TLS_CERT)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key for the public listener (env WOCA_TLS_KEY)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow plain HTTP on a public bind (development only)")
	cmd.Flags().BoolVar(&behindProxy, "behind-proxy", false, "trust X-Forwarded-* from a TLS-terminating front (bind loopback/internal)")
	for _, env := range []struct{ flag, key string }{
		{"addr", "WOCA_RELAY_ADDR"},
		{"tls-cert", "WOCA_TLS_CERT"},
		{"tls-key", "WOCA_TLS_KEY"},
	} {
		if v := os.Getenv(env.key); v != "" {
			_ = cmd.Flags().Set(env.flag, v)
		}
	}
	return cmd
}
