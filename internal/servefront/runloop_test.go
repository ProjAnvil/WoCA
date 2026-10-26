package servefront

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestServeRunLoopSeam: an edition's RunLoop is invoked exactly once, in its
// own goroutine, with the fully-built deps — agent, settings store, run
// ledger, shared session dir, logger; the plane/obs/generation fields stay
// nil for an edition that injects none of them. The nil-RunLoop regression
// is every other test in this package (they all run the zero Edition).
func TestServeRunLoopSeam(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	depsCh := make(chan RunLoopDeps, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeWithEdition(ctx, opts, addr, true, "", Edition{
			RunLoop: func(ctx context.Context, deps RunLoopDeps) {
				depsCh <- deps
			},
		})
	}()

	select {
	case deps := <-depsCh:
		if deps.Agent == nil {
			t.Error("deps.Agent = nil, want the built agent")
		}
		if deps.Settings == nil {
			t.Error("deps.Settings = nil, want the open settings store")
		}
		if deps.Ledger == nil {
			t.Error("deps.Ledger = nil, want the run ledger (WOCA_DB is set)")
		}
		if deps.SessionDir == "" {
			t.Error("deps.SessionDir = \"\", want the shared session dir")
		}
		if deps.Plane != nil {
			t.Error("deps.Plane != nil without Edition.Plane")
		}
		if deps.Obs != nil {
			t.Error("deps.Obs != nil without Edition.Obs")
		}
		if deps.NacosGeneration != nil {
			t.Error("deps.NacosGeneration != nil without Edition.Nacos")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RunLoop never invoked")
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil && fmt.Sprintf("%v", err) != "context canceled" {
			t.Fatalf("Serve returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not shut down")
	}
}
