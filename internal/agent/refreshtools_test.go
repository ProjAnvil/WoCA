package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Fake slow MCP: the tool round must not block; no second refresh within the
// TTL; rebinding takes effect once the background refresh completes.
func TestMaybeRefreshToolsNonBlocking(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	a := newRunAgent(t, t.TempDir(), nil) // runws_test.go:19 helper
	a.refreshTools = func(ctx context.Context) []coretools.Tool {
		calls.Add(1)
		<-release
		return nil
	}

	done := make(chan struct{})
	go func() { a.maybeRefreshTools(context.Background()); close(done) }()
	select {
	case <-done: // must return immediately, not blocked by refreshTools
	case <-time.After(2 * time.Second):
		t.Fatal("maybeRefreshTools blocked on slow refresh")
	}
	close(release)
	// calls==1 alone is observable before the refresh finishes (the hook
	// increments on entry); refreshLast is stored only after the singleflight
	// result arrives, so waiting on both waits for actual completion.
	waitFor(t, 2*time.Second, func() bool {
		return calls.Load() == 1 && a.refreshLast.Load() != 0
	}, "first refresh completes")

	// Within the TTL: not triggered again
	a.maybeRefreshTools(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls within TTL = %d, want 1", n)
	}
}

// RefreshToolsNow bypasses the TTL and deduplicates against in-flight
// refreshes (singleflight).
func TestRefreshToolsNowBypassesTTL(t *testing.T) {
	var calls atomic.Int32
	a := newRunAgent(t, t.TempDir(), nil)
	a.refreshTools = func(ctx context.Context) []coretools.Tool { calls.Add(1); return nil }
	a.RefreshToolsNow(context.Background())
	a.RefreshToolsNow(context.Background())
	if n := calls.Load(); n != 2 {
		t.Fatalf("RefreshToolsNow calls = %d, want 2 (TTL bypassed)", n)
	}
}

// Background flights must have a timeout: a hung MCP client (no internal
// timeout) must not stall the flight forever — otherwise a synchronous
// RefreshToolsNow joining the same singleflight key blocks forever.
func TestMaybeRefreshToolsFlightTimeout(t *testing.T) {
	old := refreshFlightTimeout
	refreshFlightTimeout = 50 * time.Millisecond
	defer func() { refreshFlightTimeout = old }()
	a := newRunAgent(t, t.TempDir(), nil)
	unblocked := make(chan struct{})
	a.refreshTools = func(ctx context.Context) []coretools.Tool {
		<-ctx.Done() // hangs forever: only the flight timeout can release it
		close(unblocked)
		return nil
	}
	a.maybeRefreshTools(context.Background())
	// The synchronous path joins the in-flight hung flight: it must be
	// released by the same timeout, not blocked forever.
	nowDone := make(chan struct{})
	go func() { a.RefreshToolsNow(context.Background()); close(nowDone) }()
	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("hung refresh hook was never released by the flight timeout")
	}
	select {
	case <-nowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshToolsNow permanently blocked on the hung flight")
	}
	waitFor(t, 2*time.Second, func() bool { return a.refreshLast.Load() != 0 },
		"flight completes after timeout")
}
