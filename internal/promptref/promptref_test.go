package promptref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandIncludesWorkspaceFile(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(path, []byte("hello from note"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Expand("read @note.txt", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<woca-file path=\"note.txt\">") {
		t.Fatalf("expanded prompt omitted file marker: %q", got)
	}
	if !strings.Contains(got, "hello from note") {
		t.Fatalf("expanded prompt omitted file contents: %q", got)
	}
}

func TestExpandLeavesSubagentMentionUntouched(t *testing.T) {
	got, err := Expand("delegate this to @reviewer", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "delegate this to @reviewer" {
		t.Fatalf("subagent mention changed to %q", got)
	}
}

func TestExpandIncludesDirectoryFilesInPathOrder(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "docs", "b.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "docs", "a.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Expand("summarize @docs", workspace)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(got, "path=\"docs/a.txt\"")
	second := strings.Index(got, "path=\"docs/b.txt\"")
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("directory contents were not expanded in path order: %q", got)
	}
}

func TestExpandRejectsReferenceOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(filepath.Dir(workspace), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	_, err := Expand("read @../outside.txt", workspace)
	if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("err = %v, want workspace-boundary error", err)
	}
}

func TestCandidatesReturnsWorkspacePathsMatchingPrefix(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "docs", "guide.md"), []byte("guide"), 0o644); err != nil {
		t.Fatal(err)
	}

	candidates := Candidates("doc", workspace)
	if len(candidates) != 2 || candidates[0].Path != "docs/" || !candidates[0].IsDir || candidates[1].Path != "docs/guide.md" || candidates[1].IsDir {
		t.Fatalf("candidates = %#v", candidates)
	}
}
