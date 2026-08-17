package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadToolSlicesAndTruncates(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	var full strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&full, "line%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "ten.txt"), []byte(full.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool, _ := reg.Get("Read")
	res, err := tool.Invoke(context.Background(), map[string]any{"file_path": "ten.txt", "offset": 2, "limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "line3") || !strings.Contains(res.Content, "line5") || strings.Contains(res.Content, "line6") {
		t.Fatalf("offset/limit slice = %q", res.Content)
	}
	if !strings.Contains(res.Content, "(truncated; 10 lines total)") {
		t.Fatalf("truncation note missing: %q", res.Content)
	}

	// offset past EOF yields the empty tail with no truncation note.
	res, err = tool.Invoke(context.Background(), map[string]any{"file_path": "ten.txt", "offset": 50})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "" {
		t.Fatalf("past-EOF read = %q, want empty", res.Content)
	}

	// limit reaching past EOF is clamped to the last line.
	res, err = tool.Invoke(context.Background(), map[string]any{"file_path": "ten.txt", "offset": 8, "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "line9") || !strings.Contains(res.Content, "line10") ||
		strings.Contains(res.Content, "line8") || strings.Contains(res.Content, "truncated") {
		t.Fatalf("clamped end = %q", res.Content)
	}

	// negative offset is clamped to 0
	res, err = tool.Invoke(context.Background(), map[string]any{"file_path": "ten.txt", "offset": -5, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "line1") || strings.Contains(res.Content, "line2") {
		t.Fatalf("clamped offset = %q", res.Content)
	}

	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "missing.txt"}); err == nil ||
		!strings.Contains(err.Error(), "read missing.txt") {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestReadToolBrokenImage(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.png"), []byte("not really a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, _ := reg.Get("Read")
	res, err := tool.Invoke(context.Background(), map[string]any{"file_path": "broken.png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "not readable as an image") {
		t.Fatalf("broken image read = %q", res.Content)
	}
}

func TestWriteToolErrorPaths(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, _ := reg.Get("Write")

	// Parent path component is a regular file: MkdirAll fails.
	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "plain/sub/x.txt", "content": "x"}); err == nil ||
		!strings.Contains(err.Error(), "write plain/sub/x.txt") {
		t.Fatalf("mkdir error = %v", err)
	}

	// Read-only directory: MkdirAll is fine (dir exists), WriteFile fails.
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not block writes")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "ro/x.txt", "content": "x"}); err == nil ||
		!strings.Contains(err.Error(), "write ro/x.txt") {
		t.Fatalf("writefile error = %v", err)
	}
}

func TestEditToolBranches(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	path := filepath.Join(dir, "multi.txt")
	if err := os.WriteFile(path, []byte("a x b\nx c x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, _ := reg.Get("Edit")

	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "missing.txt", "old_string": "a", "new_string": "b"}); err == nil ||
		!strings.Contains(err.Error(), "edit missing.txt") {
		t.Fatalf("missing file error = %v", err)
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "multi.txt", "old_string": "zzz", "new_string": "b"}); err == nil ||
		!strings.Contains(err.Error(), "old_string not found") {
		t.Fatalf("not-found error = %v", err)
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"file_path": "multi.txt", "old_string": "x", "new_string": "y"}); err == nil ||
		!strings.Contains(err.Error(), "appears 3 times") {
		t.Fatalf("ambiguous error = %v", err)
	}

	res, err := tool.Invoke(context.Background(), map[string]any{
		"file_path": "multi.txt", "old_string": "x", "new_string": "Y", "replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Edited multi.txt (3 occurrences)") {
		t.Fatalf("replace_all = %q", res.Content)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "a Y b\nY c Y\n" {
		t.Fatalf("replace_all content = %q", data)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not block writes")
	}
	// An existing file is rewritten in place, so make the file itself
	// read-only (the directory's mode would not block the rewrite).
	locked := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(locked, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"file_path": "locked.txt", "old_string": "keep", "new_string": "drop",
	}); err == nil || !strings.Contains(err.Error(), "edit locked.txt") {
		t.Fatalf("edit of read-only file = %v", err)
	}

	// Single occurrence reports "1 occurrence" even with replace_all set.
	if err := os.WriteFile(path, []byte("one mark\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Invoke(context.Background(), map[string]any{
		"file_path": "multi.txt", "old_string": "mark", "new_string": "MARK", "replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "1 occurrence") {
		t.Fatalf("single occurrence = %q", res.Content)
	}
}
