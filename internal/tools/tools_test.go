package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileAndSearchTools(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)

	invoke := func(name string, args map[string]any) string {
		t.Helper()
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		res, err := tool.Invoke(t.Context(), args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content
	}

	if got := invoke("Write", map[string]any{"file_path": "a/b.txt", "content": "hello\nworld\n"}); !strings.Contains(got, "Wrote") {
		t.Fatalf("Write = %q", got)
	}
	if got := invoke("Read", map[string]any{"file_path": "a/b.txt"}); !strings.Contains(got, "hello") {
		t.Fatalf("Read = %q", got)
	}
	if got := invoke("Edit", map[string]any{"file_path": "a/b.txt", "old_string": "hello", "new_string": "HELLO"}); !strings.Contains(got, "Edited") {
		t.Fatalf("Edit = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a/b.txt"))
	if !strings.Contains(string(data), "HELLO") {
		t.Fatalf("edit did not persist: %q", data)
	}

	if got := invoke("Glob", map[string]any{"pattern": "*.txt"}); !strings.Contains(got, "a/b.txt") {
		t.Fatalf("Glob = %q", got)
	}
	if got := invoke("Grep", map[string]any{"pattern": "HELLO", "path": "."}); !strings.Contains(got, "a/b.txt") {
		t.Fatalf("Grep = %q", got)
	}
	if got := invoke("ListDir", map[string]any{"path": "."}); !strings.Contains(got, "a/") {
		t.Fatalf("ListDir = %q", got)
	}
}

func TestTodoWrite(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	tool, _ := reg.Get("TodoWrite")
	res, err := tool.Invoke(t.Context(), map[string]any{
		"todos": []map[string]any{{"content": "do it", "status": "in_progress"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "do it") {
		t.Fatalf("TodoWrite = %q", res.Content)
	}
	if got := len(reg.TodoStore().Get()); got != 1 {
		t.Fatalf("store has %d todos, want 1", got)
	}
}
