package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchToolErrorBranches(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "("}); !strings.Contains(res.Content, "invalid regexp") {
		t.Fatalf("bad regexp = %q", res.Content)
	}
	if res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "x", "include": "[z-a]"}); !strings.Contains(res.Content, "invalid include glob") {
		t.Fatalf("bad include = %q", res.Content)
	}
	if res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "[z-a]"}); !strings.Contains(res.Content, "invalid glob") {
		t.Fatalf("bad glob = %q", res.Content)
	}
	if res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "*.txt", "path": "missing-dir"}); !strings.Contains(res.Content, "Error") {
		t.Fatalf("missing glob base = %q", res.Content)
	}
	if res := reg.Execute(t.Context(), "ListDir", map[string]any{"path": "missing-dir"}); !strings.Contains(res.Content, "Error") {
		t.Fatalf("ListDir missing = %q", res.Content)
	}
	if res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "*.zzz"}); !strings.Contains(res.Content, "No files matched") {
		t.Fatalf("glob no-match placeholder = %q", res.Content)
	}
}

func TestGlobOnFileBase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "single.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	// Base is a regular file: the single-file branch matches on basename.
	res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "single.txt", "path": "single.txt"})
	if !strings.Contains(res.Content, "single.txt") {
		t.Fatalf("glob on file base = %q", res.Content)
	}
	res = reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "other.txt", "path": "single.txt"})
	if !strings.Contains(res.Content, "No files matched") {
		t.Fatalf("glob on file base mismatch = %q", res.Content)
	}
}

func TestGrepTruncatesAtMaxResults(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := range maxSearchResults + 50 {
		fmt.Fprintf(&b, "hit %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "hit", "path": "."})
	if !strings.Contains(res.Content, fmt.Sprintf("(truncated at %d matches)", maxSearchResults)) {
		t.Fatalf("truncation note missing from %d-char result", len(res.Content))
	}
	if got := strings.Count(res.Content, ":hit "); got != maxSearchResults {
		t.Fatalf("matched lines = %d, want %d", got, maxSearchResults)
	}
}

func TestGrepMissingBaseError(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "x", "path": "missing-dir"})
	if !strings.Contains(res.Content, "Error") || !strings.Contains(res.Content, "missing-dir") {
		t.Fatalf("grep missing base = %q", res.Content)
	}
}

func TestGrepTruncatesAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		var b strings.Builder
		for i := range 100 {
			fmt.Fprintf(&b, "needle %d\n", i)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "needle", "path": "."})
	if !strings.Contains(res.Content, fmt.Sprintf("(truncated at %d matches)", maxSearchResults)) {
		t.Fatalf("truncation note missing: %q", res.Content[:200])
	}
	if got := strings.Count(res.Content, ":needle "); got != maxSearchResults {
		t.Fatalf("matched lines = %d, want %d", got, maxSearchResults)
	}
	if strings.Contains(res.Content, "c.txt") {
		t.Fatal("third file should never be scanned after the cap is hit")
	}
}

func TestGrepNoMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "zzz", "path": "."})
	if res.Content != "No matches found." {
		t.Fatalf("no-match grep = %q", res.Content)
	}
}

func TestFormatMatchesTruncation(t *testing.T) {
	if got := formatMatches(nil, "nothing here"); got != "nothing here" {
		t.Fatalf("empty = %q", got)
	}
	matches := make([]string, maxSearchResults+10)
	for i := range matches {
		matches[i] = fmt.Sprintf("m%d", i)
	}
	got := formatMatches(matches, "")
	if strings.Count(got, "\n") != maxSearchResults {
		t.Fatalf("truncated match count = %d, want %d", strings.Count(got, "\n"), maxSearchResults)
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false}, // single * does not cross "/"
		{"**/*.go", "main.go", true},
		{"**/*.go", "deep/nest/main.go", true},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/deep/main.go", true},
		{"src/**/*.go", "srcmain.go", false},
		{"**/**/main.go", "main.go", true},
		{"a**/main.go", "amain.go", false},
		{"a**/main.go", "abc/main.go", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"[abc].txt", "b.txt", true},
		{"[abc].txt", "d.txt", false},
		{"a[b.txt", "a[b.txt", true}, // unclosed class is literal
		{"a[b.txt", "ab.txt", false},
		{"x+.txt", "x+.txt", true}, // + is quoted
		{"\u62a5\u544a.md", "\u62a5\u544a.md", true},
		{"\u8d44\u6599/*.md", "\u8d44\u6599/\u62a5\u544a.md", true},
		{"\u8d44\u6599/*.md", "\u5176\u4ed6/\u62a5\u544a.md", false},
		{"\U0001f4c4?.md", "\U0001f4c4\u62a5.md", true},
		{"\u62a5+.md", "\u62a5+.md", true},
		{"\u62a5+.md", "\u62a5\u62a5.md", false},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.pattern)
		if err != nil {
			t.Errorf("globToRegexp(%q): %v", c.pattern, err)
			continue
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q match %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
	if _, err := globToRegexp("[z-a]"); err == nil {
		t.Error("globToRegexp([z-a]) = nil error, want invalid class")
	}
}

func TestSearchToolsUnicodePatterns(t *testing.T) {
	dir := t.TempDir()
	if err := mkdirAllWrite(filepath.Join(dir, "\u8d44\u6599", "\u62a5\u544a.md"), "needle\n"); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllWrite(filepath.Join(dir, "other.md"), "needle\n"); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{
			name: "glob_basename",
			tool: "Glob",
			args: map[string]any{"pattern": "\u62a5\u544a.md"},
			want: "\u8d44\u6599/\u62a5\u544a.md\n",
		},
		{
			name: "glob_directory",
			tool: "Glob",
			args: map[string]any{"pattern": "\u8d44\u6599/*.md"},
			want: "\u8d44\u6599/\u62a5\u544a.md\n",
		},
		{
			name: "grep_include_directory",
			tool: "Grep",
			args: map[string]any{"pattern": "needle", "include": "\u8d44\u6599/*.md"},
			want: "\u8d44\u6599/\u62a5\u544a.md:1:needle\n",
		},
		{
			name: "grep_include_basename",
			tool: "Grep",
			args: map[string]any{"pattern": "needle", "include": "**/\u62a5\u544a.md"},
			want: "\u8d44\u6599/\u62a5\u544a.md:1:needle\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := reg.Execute(t.Context(), tc.tool, tc.args); res.Content != tc.want {
				t.Fatalf("%s = %q, want %q", tc.tool, res.Content, tc.want)
			}
		})
	}
}

func TestSearchToolsGlobstarMatchesRootAndNestedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"main.go", "pkg/main.go", "pkg/deep/main.go", "pkg/other.txt"} {
		if err := mkdirAllWrite(filepath.Join(dir, filepath.FromSlash(name)), "needle\n"); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewRegistry(dir)
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{
			name: "glob_from_root",
			tool: "Glob",
			args: map[string]any{"pattern": "**/*.go"},
			want: "main.go\npkg/deep/main.go\npkg/main.go\n",
		},
		{
			name: "glob_from_subdirectory",
			tool: "Glob",
			args: map[string]any{"pattern": "pkg/**/*.go"},
			want: "pkg/deep/main.go\npkg/main.go\n",
		},
		{
			name: "grep_from_root",
			tool: "Grep",
			args: map[string]any{"pattern": "needle", "include": "**/*.go"},
			want: "main.go:1:needle\npkg/deep/main.go:1:needle\npkg/main.go:1:needle\n",
		},
		{
			name: "grep_from_subdirectory",
			tool: "Grep",
			args: map[string]any{"pattern": "needle", "include": "pkg/**/*.go"},
			want: "pkg/deep/main.go:1:needle\npkg/main.go:1:needle\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := reg.Execute(t.Context(), tc.tool, tc.args); res.Content != tc.want {
				t.Fatalf("%s = %q, want %q", tc.tool, res.Content, tc.want)
			}
		})
	}
}

func TestGlobDeepPatternMatchesBasenames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deep", "nest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deep", "nest", "target.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "Glob", map[string]any{"pattern": "*.md"})
	if !strings.Contains(res.Content, "deep/nest/target.md") {
		t.Fatalf("deep glob = %q", res.Content)
	}
	res = reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "x", "include": "**/*.md", "path": "."})
	if !strings.Contains(res.Content, "deep/nest/target.md") {
		t.Fatalf("grep include filter = %q", res.Content)
	}
	res = reg.Execute(t.Context(), "Grep", map[string]any{"pattern": "x", "include": "**/*.txt", "path": "."})
	if res.Content != "No matches found." {
		t.Fatalf("grep include filtered everything = %q", res.Content)
	}
}
