package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// fake 慢 MCP：工具轮不被阻塞；TTL 内不二次刷新；后台完成后换绑生效。
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
	case <-done: // 必须立即返回，不被 refreshTools 阻塞
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

	// TTL 内：不再触发
	a.maybeRefreshTools(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls within TTL = %d, want 1", n)
	}
}

// RefreshToolsNow 绕过 TTL，且与在途刷新去重（singleflight）。
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
