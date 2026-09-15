package agent

import (
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/store"
)

// Two concurrent runs doing Begin/TakeAttachments each must not overwrite one
// another (reworked from the original case that acknowledged the clobbering).
func TestMemoryScopeIsolatesConcurrentRuns(t *testing.T) {
	m := &memoryRuntime{}
	ctxA := WithMemoryScope(t.Context())
	ctxB := WithMemoryScope(t.Context())

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
	// A second take is empty (Take clears the scope)
	if again := m.TakeAttachments(ctxA); len(again) != 0 {
		t.Fatalf("second take = %+v, want empty", again)
	}
}

// Without a scope in ctx, fall back to agent-level fields (the CLI single-run
// path must not regress).
func TestTakeAttachmentsFallbackWithoutScope(t *testing.T) {
	m := &memoryRuntime{}
	m.prefetch.entries = []store.Entry{{Namespace: "ns", Key: "x", Value: []byte(`"legacy"`)}}
	got := m.TakeAttachments(t.Context())
	if len(got) != 1 {
		t.Fatalf("fallback take = %+v, want 1 attachment", got)
	}
}
