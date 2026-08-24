// woca-edge is the edge access point (product design v1.5 拓扑拆分): the
// control plane's only public component — a stateless router (R7: no
// database, no files, no secrets; every machine-token check calls home to
// the service plane). It terminates client traffic into the bridges'
// outbound tunnels (scenario four) and forwards the machine channel into
// the private core: config pulls to the config plane, everything else to
// the service plane.
//
// Deliberately thin: no agent kernel, no tools, no sqlite — the binary
// must stay small enough that its attack surface is the only thing it
// exposes.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/ee/edgegw"
	"github.com/projanvil/woca/internal/logging"
	"github.com/spf13/cobra"
)

func main() {
	var logLevel, addr, tlsCert, tlsKey, serviceURL, configURL string
	root := &cobra.Command{
		Use:           "woca-edge",
		Short:         "woca edge access point (stateless public entry)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log := logging.NewConsole(os.Stderr, level)
			if serviceURL == "" {
				return fmt.Errorf("the service plane is required (set --service-url or WOCA_SERVICE_URL)")
			}
			edge := edgegw.New(edgegw.Config{ServiceURL: serviceURL, ConfigURL: configURL}, log)

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			scheme := "http"
			if tlsCert != "" && tlsKey != "" {
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
			} else if isPublicBind(addr) && os.Getenv("WOCA_EDGE_DEV_HTTP") != "1" {
				// R1 posture for the public entry: plain HTTP on a public
				// bind is the same shell-to-the-internet mistake the
				// bridge red line forbids. Loopback/dev binds stay HTTP;
				// the dev compose stacks opt in explicitly.
				return fmt.Errorf("refusing to serve plain HTTP on a non-loopback bind (set --tls-cert/--tls-key, or WOCA_EDGE_DEV_HTTP=1 for dev stacks)")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go edge.Run(ctx)

			hs := &http.Server{
				Addr:              addr,
				Handler:           edge.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
			}
			serveErr := make(chan error, 1)
			go func() { serveErr <- hs.Serve(ln) }()
			fmt.Fprintf(os.Stderr, "woca-edge: %s://%s (service %s, config %s)\n", scheme, ln.Addr(), serviceURL, configURL)
			select {
			case err := <-serveErr:
				if err == http.ErrServerClosed {
					return nil
				}
				return err
			case <-ctx.Done():
				// Parked clients wait up to parkPickupTimeout (30s) for a
				// tunnel pickup and SSE relays stream as long as the bridge
				// answers — a 5s grace would sever both mid-flight.
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				return hs.Shutdown(shutdownCtx)
			}
		},
	}
	root.Flags().StringVar(&logLevel, "log-level", "info", "debug | info | warn | error")
	root.Flags().StringVar(&addr, "addr", ":443", "public listen address (env WOCA_EDGE_ADDR)")
	root.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate for the public listener (env WOCA_TLS_CERT)")
	root.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key for the public listener (env WOCA_TLS_KEY)")
	root.Flags().StringVar(&serviceURL, "service-url", "", "service plane base URL, private network (env WOCA_SERVICE_URL)")
	root.Flags().StringVar(&configURL, "config-url", "", "config plane base URL, private network (env WOCA_CONFIGSVC_URL)")
	for _, env := range []struct{ flag, key string }{
		{"addr", "WOCA_EDGE_ADDR"},
		{"tls-cert", "WOCA_TLS_CERT"},
		{"tls-key", "WOCA_TLS_KEY"},
		{"service-url", "WOCA_SERVICE_URL"},
		{"config-url", "WOCA_CONFIGSVC_URL"},
	} {
		if v := os.Getenv(env.key); v != "" {
			_ = root.Flags().Set(env.flag, v)
		}
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// isPublicBind reports whether the bind address reaches beyond loopback.
func isPublicBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return true // ":port" binds every interface
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}
