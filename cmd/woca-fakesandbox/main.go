// Command woca-fakesandbox is the containerizable form of the in-memory
// sandbox contract v2 fake (internal/platform/executor/fakesandbox): a
// thin main that serves the fake's Handler so docker-based e2e stacks
// (e2e/sandboxlab) can point a contract-http backend at a real container.
//
// Test-only artifact: it is deliberately NOT part of SERVER_ARTIFACTS —
// `make dist-role ROLE=woca-fakesandbox` still bakes it through the same
// generic packaging (build/dist/Dockerfile), which is exactly how the
// sandboxlab compose consumes it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
)

func main() {
	addr := flag.String("addr", envOr("WOCA_FAKESANDBOX_ADDR", ":8080"), "listen address (env WOCA_FAKESANDBOX_ADDR)")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "woca-fakesandbox: listen %s: %v\n", *addr, err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: fakesandbox.New().Handler()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	fmt.Fprintf(os.Stderr, "woca-fakesandbox: %s (contract v2 fake, in-memory)\n", *addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	_ = srv.Close()
	<-done
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
