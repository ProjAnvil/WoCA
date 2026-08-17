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

// 后台 flight 必须有超时：挂死的 MCP client（无内部超时）不能让 flight 永不
// 完成——否则同步的 RefreshToolsNow 加入同一 singleflight key 后永久阻塞。
func TestMaybeRefreshToolsFlightTimeout(t *testing.T) {
	old := refreshFlightTimeout
	refreshFlightTimeout = 50 * time.Millisecond
	defer func() { refreshFlightTimeout = old }()
	a := newRunAgent(t, t.TempDir(), nil)
	unblocked := make(chan struct{})
	a.refreshTools = func(ctx context.Context) []coretools.Tool {
		<-ctx.Done() // 挂死：只有 flight 超时能解开
		close(unblocked)
		return nil
	}
	a.maybeRefreshTools(context.Background())
	// 同步路径加入在途的挂死 flight：必须随超时一起解开，而非永久阻塞。
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
