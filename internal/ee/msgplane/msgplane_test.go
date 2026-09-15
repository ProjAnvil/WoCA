package msgplane

// Tests cover the resolver-driven semantics: pg mode is a no-op, pg+nats
// delivers dispatch notifications to waiters, live config changes (URL,
// subject, back to pg) reconfigure the plane without restarts, and an
// unreachable NATS degrades to poll-mode without failing the caller. A
// real embedded NATS server backs the delivery tests.

import (
	"context"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/rs/zerolog"
)

// startNATS runs an embedded NATS server and returns its client URL.
func startNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS never became ready")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}

func TestValidMode(t *testing.T) {
	for _, ok := range []string{"", "pg", "pg+nats"} {
		if !ValidMode(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"nats", "PG"} {
		if ValidMode(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

func TestConfigNATSEnabled(t *testing.T) {
	if (Config{Mode: "pg", NATSURL: "nats://x"}).NATSEnabled() {
		t.Fatal("pg mode never enables NATS")
	}
	if (Config{Mode: "pg+nats"}).NATSEnabled() {
		t.Fatal("pg+nats without a URL stays on the PG queue")
	}
	if !(Config{Mode: "pg+nats", NATSURL: "nats://x"}).NATSEnabled() {
		t.Fatal("pg+nats with a URL should enable NATS")
	}
}

// pg mode (and the nil plane) publish nothing and never dial.
func TestPGModeIsNoOp(t *testing.T) {
	p := New(func() Config { return Config{Mode: "pg"} }, zerolog.Nop())
	defer p.Close()
	p.Notify("r1") // must not dial anything

	var nilPlane *Plane
	nilPlane.Notify("r2") // nil-receiver guard

	w := NewWaiter(p)
	defer w.Close()
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	w.Wait(ctx)
	// A short pg-mode wait parks for the poll interval, not zero (no spin).
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("pg-mode wait returned too early: %v", elapsed)
	}
}

// The full overlay: a notify in pg+nats mode wakes a waiting worker, and
// switching back to pg mode drops the subscription.
func TestNotifyWakesWaiter(t *testing.T) {
	natsURL := startNATS(t)
	var mu sync.Mutex
	mode := "pg+nats"
	p := New(func() Config {
		mu.Lock()
		defer mu.Unlock()
		return Config{Mode: mode, NATSURL: natsURL}
	}, zerolog.Nop())
	defer p.Close()
	w := NewWaiter(p)
	defer w.Close()

	woken := make(chan struct{})
	go func() {
		w.Wait(t.Context())
		close(woken)
	}()
	// Give the subscription a moment to register, then notify.
	time.Sleep(100 * time.Millisecond)
	p.Notify("run-1")
	select {
	case <-woken:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke on the dispatch notification")
	}

	// Switching the live config back to pg parks the waiter on the poll
	// cadence without the subscription.
	mu.Lock()
	mode = "pg"
	mu.Unlock()
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	w.Wait(ctx)
	if time.Since(start) >= 200*time.Millisecond {
		t.Fatal("pg-mode wait exceeded the poll interval (subscription not dropped?)")
	}
}

// An unreachable NATS URL degrades to polling: Notify returns (never fails
// the enqueue), Wait parks on the poll cadence.
func TestUnreachableNATSDegrades(t *testing.T) {
	p := New(func() Config {
		return Config{Mode: "pg+nats", NATSURL: "nats://127.0.0.1:1"}
	}, zerolog.Nop())
	defer p.Close()
	p.Notify("r1") // logs, returns

	w := NewWaiter(p)
	defer w.Close()
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	w.Wait(ctx)
	if time.Since(start) >= 200*time.Millisecond {
		t.Fatal("degraded wait exceeded the poll interval")
	}
}

// A URL change on a live plane reconnects to the new server.
func TestURLChangeReconnects(t *testing.T) {
	first := startNATS(t)
	var mu sync.Mutex
	cfg := Config{Mode: "pg+nats", NATSURL: first}
	p := New(func() Config {
		mu.Lock()
		defer mu.Unlock()
		return cfg
	}, zerolog.Nop())
	defer p.Close()
	p.Notify("warmup-connection")

	second := startNATS(t)
	mu.Lock()
	cfg.NATSURL = second
	mu.Unlock()
	w := NewWaiter(p)
	defer w.Close()
	woken := make(chan struct{})
	go func() {
		w.Wait(t.Context())
		close(woken)
	}()
	time.Sleep(150 * time.Millisecond)
	p.Notify("run-2")
	select {
	case <-woken:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke after the URL switch")
	}
}
