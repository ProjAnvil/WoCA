package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/store"
)

// 并发两 run 各自 Begin/TakeAttachments 互不覆盖（改造原承认互踩的用例）。
func TestMemoryScopeIsolatesConcurrentRuns(t *testing.T) {
	m := &memoryRuntime{}
	ctxA := WithMemoryScope(context.Background())
	ctxB := WithMemoryScope(context.Background())

	scopeA, scopeB := memoryScopeFrom(ctxA), memoryScopeFrom(ctxB)
	scopeA.entries = []store.Entry{{Namespace: "ns", Key: "a-1", Value: []byte(`"alpha"`)}}
	scopeB.entries = []store.Entry{{Namespace: "ns", Key: "b-1", Value: []byte(`"beta"`)}}

	gotA := m.TakeAttachments(ctxA)
	if len(gotA) != 1 || !strings.Contains(gotA[0].Content, "alpha") {
		t.Fatalf("run A got %+v, want alpha attachment", gotA)
	}
	gotB := m.TakeAttachments(ctxB)
	if len(gotB) != 1 || !strings.Contains(gotB[0].Content, "beta") {
		t.Fatalf("run B got %+v, want beta attachment", gotB)
	}
	// 二次取为空（Take 清空 scope）
	if again := m.TakeAttachments(ctxA); len(again) != 0 {
		t.Fatalf("second take = %+v, want empty", again)
	}
}

// ctx 无 scope 时回退 agent 级字段（CLI 单 run 路径不回归）。
func TestTakeAttachmentsFallbackWithoutScope(t *testing.T) {
	m := &memoryRuntime{}
	m.prefetch.entries = []store.Entry{{Namespace: "ns", Key: "x", Value: []byte(`"legacy"`)}}
	got := m.TakeAttachments(context.Background())
	if len(got) != 1 {
		t.Fatalf("fallback take = %+v, want 1 attachment", got)
	}
}
