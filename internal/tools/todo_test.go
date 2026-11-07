package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/tracing"
)

// TestTodoWriteSessionsAreIsolated reproduces the process-global todo leak:
// two run scopes writing through TodoWrite must not see or overwrite each
// other's lists — each session's tool result echoes only its own list.
func TestTodoWriteSessionsAreIsolated(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("TodoWrite")
	ctxA := tracing.WithRun(t.Context(), "run-A", "sess-A")
	ctxB := tracing.WithRun(t.Context(), "run-B", "sess-B")

	if _, err := tool.Invoke(ctxA, map[string]any{
		"todos": []map[string]any{{"content": "A1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Invoke(ctxB, map[string]any{
		"todos": []map[string]any{{"content": "B1"}},
	}); err != nil {
		t.Fatal(err)
	}
	// A merges a new item: with a process-global store A's earlier item was
	// destroyed by B's write and B's item leaks into A's echo.
	res, err := tool.Invoke(ctxA, map[string]any{
		"todos": []map[string]any{{"content": "A2"}},
		"merge": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"A1", "A2"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("session A's TodoWrite lost its own item %q: %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "B1") {
		t.Fatalf("session A's TodoWrite echoed session B's todos: %q", res.Content)
	}
	// B's echo must likewise show only B's list.
	resB, err := tool.Invoke(ctxB, map[string]any{
		"todos": []map[string]any{{"content": "B1", "status": "completed"}},
		"merge": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resB.Content, "A1") || strings.Contains(resB.Content, "A2") {
		t.Fatalf("session B's TodoWrite echoed session A's todos: %q", resB.Content)
	}
	if !strings.Contains(resB.Content, "[x] B1") {
		t.Fatalf("session B's TodoWrite missing its own todos: %q", resB.Content)
	}
}

func TestTodoStoreMerge(t *testing.T) {
	s := NewTodoStore()
	s.Set("", []Todo{
		{Content: "first", Status: "completed"},
		{Content: "second", Status: "in_progress", ActiveForm: "working on second"},
	})
	s.Merge("", []Todo{
		{Content: "second", Status: "completed"},                 // updates in place
		{Content: "third", Status: "pending"},                    // appended
		{Content: "second", Status: "cancelled", ActiveForm: ""}, // updates again
	})
	got, _, ok := s.Get("")
	if !ok || len(got) != 3 {
		t.Fatalf("Merge produced %d todos (ok=%v), want 3: %+v", len(got), ok, got)
	}
	if got[0].Content != "first" || got[0].Status != "completed" {
		t.Fatalf("first changed by merge: %+v", got[0])
	}
	if got[1].Status != "cancelled" {
		t.Fatalf("second not updated by merge: %+v", got[1])
	}
	if got[2].Content != "third" {
		t.Fatalf("third not appended: %+v", got[2])
	}
}

func TestFormatTodos(t *testing.T) {
	if got := formatTodos(nil); got != "(no todos)" {
		t.Fatalf("formatTodos(nil) = %q", got)
	}
	got := formatTodos([]Todo{
		{Content: "a"},
		{Content: "b", Status: "in_progress"},
		{Content: "c", Status: "completed"},
		{Content: "d", Status: "cancelled"},
		{Content: "e", Status: "weird", ActiveForm: "doing e"},
	})
	want := "[ ] a\n[>] b\n[x] c\n[-] d\n[ ] doing e\n"
	if got != want {
		t.Fatalf("formatTodos = %q, want %q", got, want)
	}
}

func TestTodoWriteMergeMode(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("TodoWrite")
	if _, err := tool.Invoke(t.Context(), map[string]any{
		"todos": []map[string]any{{"content": "a"}, {"content": "b"}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(t.Context(), map[string]any{
		"todos": []map[string]any{{"content": "b", "status": "completed"}, {"content": "c"}},
		"merge": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "[x] b") || !strings.Contains(res.Content, "a") || !strings.Contains(res.Content, "c") {
		t.Fatalf("merged TodoWrite output = %q", res.Content)
	}
	if got, _, _ := reg.TodoStore().Get(""); len(got) != 3 {
		t.Fatalf("store after merge has %d todos, want 3", len(got))
	}
}

// TestTodoStoreSessionIsolation checks the store-level bucketing: Set/Merge
// on one session never reads or writes another session's list, and a scoped
// session never observes the "" (TUI/REPL) list or vice versa.
func TestTodoStoreSessionIsolation(t *testing.T) {
	s := NewTodoStore()
	s.Set("sess-A", []Todo{{Content: "A1", Status: "pending"}})
	s.Set("sess-B", []Todo{{Content: "B1", Status: "pending"}})
	s.Set("", []Todo{{Content: "TUI", Status: "pending"}})

	// Merge only affects the merging session.
	s.Merge("sess-A", []Todo{{Content: "A2", Status: "in_progress"}})
	a, _, ok := s.Get("sess-A")
	if !ok || len(a) != 2 || a[0].Content != "A1" || a[1].Content != "A2" {
		t.Fatalf("sess-A after merge = %+v (ok=%v), want [A1 A2]", a, ok)
	}
	for _, session := range []string{"sess-B", ""} {
		got, _, _ := s.Get(session)
		if len(got) != 1 {
			t.Fatalf("session %q changed by sess-A's merge: %+v", session, got)
		}
	}

	// Set replaces only the setting session's list.
	s.Set("sess-B", []Todo{{Content: "B2", Status: "completed"}})
	b, _, _ := s.Get("sess-B")
	if len(b) != 1 || b[0].Content != "B2" {
		t.Fatalf("sess-B after set = %+v, want [B2]", b)
	}
	if a, _, _ = s.Get("sess-A"); len(a) != 2 {
		t.Fatalf("sess-A changed by sess-B's set: %+v", a)
	}

	// Drop removes only the dropped session's list.
	s.Drop("sess-A")
	if _, _, ok := s.Get("sess-A"); ok {
		t.Fatal("sess-A still present after Drop")
	}
	if _, _, ok := s.Get("sess-B"); !ok {
		t.Fatal("sess-B lost its list when sess-A was dropped")
	}
}

// TestTodoStoreGetDistinguishesNeverWrittenFromCleared pins the ok flag:
// a session that never wrote has ok=false; one that cleared its list has
// ok=true with an empty slice.
func TestTodoStoreGetDistinguishesNeverWrittenFromCleared(t *testing.T) {
	s := NewTodoStore()
	if todos, updated, ok := s.Get("sess-A"); ok {
		t.Fatalf("Get on never-written session = (%+v, %v, true), want ok=false", todos, updated)
	}
	s.Set("sess-A", []Todo{{Content: "only", Status: "pending"}})
	s.Set("sess-A", nil) // cleared, not forgotten
	todos, _, ok := s.Get("sess-A")
	if !ok || len(todos) != 0 {
		t.Fatalf("Get after clear = (%+v, ok=%v), want empty list with ok=true", todos, ok)
	}
	if _, _, ok := s.Get("sess-B"); ok {
		t.Fatal("Get on never-written sess-B = ok, want ok=false")
	}
}

// TestTodoStoreUpdatedAt checks that every write stamps the list's
// last-update time.
func TestTodoStoreUpdatedAt(t *testing.T) {
	s := NewTodoStore()
	before := time.Now().Add(-time.Second)
	s.Set("sess-A", []Todo{{Content: "a"}})
	_, updated, ok := s.Get("sess-A")
	if !ok || updated.Before(before) {
		t.Fatalf("Set did not stamp updatedAt: %v (ok=%v)", updated, ok)
	}
	first := updated
	s.Merge("sess-A", []Todo{{Content: "b"}})
	_, updated, ok = s.Get("sess-A")
	if !ok || updated.Before(first) {
		t.Fatalf("Merge did not stamp updatedAt: %v, want >= %v (ok=%v)", updated, first, ok)
	}
}

// TestTodoStoreOnChange checks that Set, Merge, and Drop each fire the
// change callback with the owning session id, and that the callback runs
// outside the store lock (it may call back into the store).
func TestTodoStoreOnChange(t *testing.T) {
	s := NewTodoStore()
	var calls []string
	s.SetOnChange(func(session string) { calls = append(calls, session) })

	s.Set("sess-A", []Todo{{Content: "a"}})
	s.Merge("sess-A", []Todo{{Content: "b"}})
	s.Drop("sess-A")
	s.Drop("sess-B") // never written: no notification
	want := []string{"sess-A", "sess-A", "sess-A"}
	if len(calls) != len(want) {
		t.Fatalf("onChange calls = %v, want %v", calls, want)
	}
	for i, session := range want {
		if calls[i] != session {
			t.Fatalf("onChange calls = %v, want %v", calls, want)
		}
	}

	// The callback may read the store (it fires outside the lock).
	s.SetOnChange(func(session string) {
		s.Get(session)
	})
	s.Set("sess-A", nil) // must not deadlock
}

// TestTodoWriteUnscopedUsesEmptyBucket pins the "" path: a TodoWrite call
// without a run scope keeps today's single-session behavior and writes the
// "" bucket, leaving scoped sessions untouched.
func TestTodoWriteUnscopedUsesEmptyBucket(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	sess := reg.TodoStore()
	sess.Set("sess-A", []Todo{{Content: "A1"}})
	tool, _ := reg.Get("TodoWrite")

	res, err := tool.Invoke(t.Context(), map[string]any{
		"todos": []map[string]any{{"content": "TUI1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "TUI1") || strings.Contains(res.Content, "A1") {
		t.Fatalf("unscoped TodoWrite echo = %q, want only TUI1", res.Content)
	}
	if todos, _, ok := sess.Get(""); !ok || len(todos) != 1 || todos[0].Content != "TUI1" {
		t.Fatalf("\"\" bucket after unscoped write = %+v (ok=%v)", todos, ok)
	}
	if todos, _, ok := sess.Get("sess-A"); !ok || len(todos) != 1 || todos[0].Content != "A1" {
		t.Fatalf("sess-A changed by unscoped write: %+v (ok=%v)", todos, ok)
	}
}
