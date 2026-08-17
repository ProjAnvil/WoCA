package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func poolKeyForTest() (string, []string) { return "/ws", []string{"fake-lsp"} }

func TestLSPPoolReuse(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := 0; i < 3; i++ {
		if err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1 (pooled reuse)", n)
	}
}

func TestLSPPoolRestartOnCrash(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	calls := 0
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
		calls++
		if calls == 1 {
			return errors.New("process died") // 首次 Call 报错 → 摘除、重启重试一次
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (one restart)", n)
	}
}

func TestLSPPoolBrokenCircuitBreaker(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return nil, errors.New("spawn failed")
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := 0; i < lspMaxSpawnFailures; i++ {
		_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	}
	// 第 3 次：熔断冷却中，直接报错且不再 spawn
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	if err == nil {
		t.Fatal("want broken error during cooldown")
	}
	if n := spawns.Load(); n != int32(lspMaxSpawnFailures) {
		t.Fatalf("spawns = %d, want %d (circuit open)", n, lspMaxSpawnFailures)
	}
}

func TestLSPPoolIdleReap(t *testing.T) {
	oldIdle, oldTick := lspIdleTimeout, lspReapInterval
	lspIdleTimeout, lspReapInterval = 30*time.Millisecond, 10*time.Millisecond
	defer func() { lspIdleTimeout, lspReapInterval = oldIdle, oldTick }()
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	time.Sleep(150 * time.Millisecond) // 等回收
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (idle entry reaped)", n)
	}
}
