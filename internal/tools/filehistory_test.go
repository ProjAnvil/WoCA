package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/runws"
)

// nilCtx satisfies Registry.Execute's context parameter in tests.
func nilCtx() context.Context { return context.Background() }

// newHistoryDir creates a temp workdir.
func newHistoryDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestFileHistoryUndoRedoCycle(t *testing.T) {
	dir := newHistoryDir(t)
	h := NewFileHistory()
	path := filepath.Join(dir, "a.txt")

	// Creation: Before is nil.
	h.Record(nilCtx(), path, nil, []byte("v1"))
	// Edit: v1 -> v2 coalesces with the creation (same path, consecutive).
	h.Record(nilCtx(), path, []byte("v1"), []byte("v2"))

	if out, err := h.Undo(nilCtx()); err != nil || out == "" {
		t.Fatalf("undo: %q %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undo of a creation must delete the file, stat err = %v", err)
	}
	// The coalesced step means a second undo has nothing for this file.
	if n, _ := h.Counts(nilCtx()); n != 0 {
		t.Fatalf("creation+edit should coalesce to one undo step, undoable = %d", n)
	}

	if out, err := h.Redo(nilCtx()); err != nil || out == "" {
		t.Fatalf("redo: %q %v", out, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v2" {
		t.Fatalf("redo restored %q, want v2 (latest After)", got)
	}
}

func TestFileHistorySeparateStepsPerFile(t *testing.T) {
	dir := newHistoryDir(t)
	h := NewFileHistory()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	h.Record(nilCtx(), a, []byte("a0"), []byte("a1"))
	h.Record(nilCtx(), b, []byte("b0"), []byte("b1"))
	if n, _ := h.Counts(nilCtx()); n != 2 {
		t.Fatalf("undoable = %d, want 2", n)
	}

	if _, err := h.Undo(nilCtx()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(b)
	if string(got) != "b0" {
		t.Fatalf("b = %q after undo, want b0 (LIFO)", got)
	}
	// A new change clears the redo stack.
	h.Record(nilCtx(), a, []byte("a1"), []byte("a2"))
	if _, r := h.Counts(nilCtx()); r != 0 {
		t.Fatalf("redoable = %d after new change, want 0", r)
	}
}

func TestRegistryUndoRestoresMode(t *testing.T) {
	dir := newHistoryDir(t)
	reg := NewRegistry(dir)
	h := reg.FileHistory()

	path := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := reg.Execute(nilCtx(), "Write", map[string]any{"file_path": path, "content": "replaced"})
	if strings.HasPrefix(res.Content, "Error") {
		t.Fatalf("Write failed: %s", res.Content)
	}
	if out, err := h.Undo(nilCtx()); err != nil || out == "" {
		t.Fatalf("undo: %q %v", out, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "original" {
		t.Fatalf("content = %q, want original", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved: %v %v", fi.Mode(), err)
	}

	// Edit path records and undoes too.
	res = reg.Execute(nilCtx(), "Edit", map[string]any{"file_path": path, "old_string": "original", "new_string": "edited"})
	if strings.HasPrefix(res.Content, "Error") {
		t.Fatalf("Edit failed: %s", res.Content)
	}
	if _, err := h.Undo(nilCtx()); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "original" {
		t.Fatalf("content = %q after edit undo, want original", got)
	}
}

func TestUndoRedoEmpty(t *testing.T) {
	h := NewFileHistory()
	if out, err := h.Undo(nilCtx()); err != nil || out != "nothing to undo" {
		t.Fatalf("empty undo: %q %v", out, err)
	}
	if out, err := h.Redo(nilCtx()); err != nil || out != "nothing to redo" {
		t.Fatalf("empty redo: %q %v", out, err)
	}
}

func TestUndoApplyFailureKeepsStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileHistory()
	h.Record(nilCtx(), path, nil, []byte("new"))

	// Make the restore unwritable: the undo must fail but leave the step in
	// the undo stack instead of losing it between the stacks.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := h.Undo(nilCtx()); err == nil {
		t.Fatal("undo should fail on an unwritable directory")
	}
	undo, redo := h.Counts(nilCtx())
	if undo != 1 || redo != 0 {
		t.Fatalf("counts after failed undo = %d/%d, want 1/0 (step must stay undoable)", undo, redo)

	}

	// Once writable again the same step undoes cleanly.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Undo(nilCtx()); err != nil {
		t.Fatalf("retry undo: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be removed after undo, stat err = %v", err)
	}
}

func TestFileHistoryScopedPerRunWorkspace(t *testing.T) {
	dirA := newHistoryDir(t)
	dirB := newHistoryDir(t)
	h := NewFileHistory()
	ctxA := runws.With(context.Background(), dirA)
	ctxB := runws.With(context.Background(), dirB)

	pathA := filepath.Join(dirA, "a.txt")
	pathB := filepath.Join(dirB, "b.txt")
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(pathA, "A1")
	h.Record(ctxA, pathA, nil, []byte("A1"))
	write(pathB, "B1")
	h.Record(ctxB, pathB, nil, []byte("B1"))

	// Undoing A's scope must not touch B's stack or files.
	if out, err := h.Undo(ctxA); err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("undo A = %q, %v", out, err)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatal("undo A should remove the file it created")
	}
	data, err := os.ReadFile(pathB)
	if err != nil || string(data) != "B1" {
		t.Fatalf("B's file must be untouched, got %q, %v", data, err)
	}
	if undo, redo := h.Counts(ctxA); undo != 0 || redo != 1 {
		t.Fatalf("A counts = %d/%d, want 0/1", undo, redo)
	}
	if undo, redo := h.Counts(ctxB); undo != 1 || redo != 0 {
		t.Fatalf("B counts = %d/%d, want 1/0", undo, redo)
	}

	// The undecorated (default) scope stays separate from both.
	if out, _ := h.Undo(context.Background()); out != "nothing to undo" {
		t.Fatalf("default scope undo = %q, want nothing to undo", out)
	}
}
