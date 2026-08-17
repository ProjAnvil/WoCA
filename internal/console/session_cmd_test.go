package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/tools"
)

func TestSessionExportAndRename(t *testing.T) {
	c, _ := newFullConsole(t)
	dir := c.deps.SessionDir
	// One saved session file.
	if err := writeSessionFile(dir, "s1", []messages.Message{messages.Human("hi"), messages.AI("hello")}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := &Session{History: []messages.Message{messages.Human("live turn")}}

	res, err := c.Execute(ctx, "export", []string{filepath.Join(dir, "s1.jsonl")}, s)
	out := res.Output
	if err != nil || !strings.Contains(out, "exported") {
		t.Fatalf("export = %q %v", out, err)
	}
	// Export with no argument exports the live session.
	res, err = c.Execute(ctx, "export", nil, s)
	out = res.Output
	if err != nil || !strings.Contains(out, "exported") {
		t.Fatalf("live export = %q %v", out, err)
	}
	// A missing file falls back to the live session instead of erroring.
	resMissing, err := c.Execute(ctx, "export", []string{filepath.Join(dir, "missing.jsonl")}, s)
	if err != nil {
		t.Fatalf("missing-file export = %+v %v", resMissing, err)
	}

	// Rename saves the live session under a sanitized name.
	res, err = c.Execute(ctx, "rename", []string{"my-session"}, s)
	if err != nil {
		t.Fatalf("rename = %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-session.json")); err != nil {
		t.Fatalf("renamed snapshot: %v", err)
	}
	// Usage and validation errors.
	if _, err := c.Execute(ctx, "rename", nil, s); err == nil {
		t.Fatal("expected usage error")
	}
	if _, err := c.Execute(ctx, "rename", []string{"..."}, s); err == nil {
		t.Fatal("expected invalid-name error")
	}
	empty := &Session{}
	if _, err := c.Execute(ctx, "rename", []string{"x"}, empty); err == nil {
		t.Fatal("expected empty-history error")
	}
}

func TestSessionExportVariants(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{History: []messages.Message{
		messages.Human("question"),
		messages.AI("answer"),
		messages.Tool("t1", "tool output"),
	}}
	// Empty conversation: error.
	if _, err := c.Execute(ctx, "export", nil, &Session{}); err == nil {
		t.Fatal("expected empty export error")
	}
	// JSON export path with todos/tasks attached.
	c.deps.Agent.SetTodos([]tools.Todo{{Content: "todo", Status: "in_progress"}})
	res, err := c.Execute(ctx, "export", []string{"snapshot.json"}, s)
	if err != nil || !strings.Contains(res.Output, "resumable JSON") {
		t.Fatalf("json export = %+v %v", res, err)
	}
	data, err := os.ReadFile(filepath.Join(c.deps.Cfg.WorkDir, "snapshot.json"))
	if err != nil || !strings.Contains(string(data), "tool output") {
		t.Fatalf("json content: %v", err)
	}
	// Markdown rendering includes tool results.
	res, err = c.Execute(ctx, "export", nil, s)
	if err != nil || !strings.Contains(res.Output, "markdown") {
		t.Fatalf("md export = %+v %v", res, err)
	}
	// An unwritable target errors.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, "export", []string{filepath.Join(blocker, "sub", "out.md")}, s); err == nil {
		t.Fatal("expected export write error")
	}
}
