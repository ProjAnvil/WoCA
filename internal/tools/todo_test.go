package tools

import (
	"context"
	"strings"
	"testing"
)

func TestTodoStoreMerge(t *testing.T) {
	s := NewTodoStore()
	s.Set([]Todo{
		{Content: "first", Status: "completed"},
		{Content: "second", Status: "in_progress", ActiveForm: "working on second"},
	})
	s.Merge([]Todo{
		{Content: "second", Status: "completed"},           // updates in place
		{Content: "third", Status: "pending"},              // appended
		{Content: "second", Status: "cancelled", ActiveForm: ""}, // updates again
	})
	got := s.Get()
	if len(got) != 3 {
		t.Fatalf("Merge produced %d todos, want 3: %+v", len(got), got)
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
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"todos": []map[string]any{{"content": "a"}, {"content": "b"}},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(context.Background(), map[string]any{
		"todos": []map[string]any{{"content": "b", "status": "completed"}, {"content": "c"}},
		"merge": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "[x] b") || !strings.Contains(res.Content, "a") || !strings.Contains(res.Content, "c") {
		t.Fatalf("merged TodoWrite output = %q", res.Content)
	}
	if got := reg.TodoStore().Get(); len(got) != 3 {
		t.Fatalf("store after merge has %d todos, want 3", len(got))
	}
}
