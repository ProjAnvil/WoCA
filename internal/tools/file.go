package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

const maxReadLines = 2000

const readDescription = `Read a UTF-8 text file and return its content with line numbers. Use offset/limit to read a slice. Truncates very large files with a note.`

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

func readTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("Read", readDescription,
		func(ctx context.Context, a readArgs) (coretools.Result, error) {
			path := resolvePath(baseWorkdir, a.FilePath)
			data, err := os.ReadFile(path)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("read %s: %w", a.FilePath, err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

			start := a.Offset
			if start < 0 {
				start = 0
			}
			end := len(lines)
			if a.Limit > 0 {
				end = start + a.Limit
				if end > len(lines) {
					end = len(lines)
				}
			}
			if start > len(lines) {
				start = len(lines)
			}
			slice := lines[start:end]

			truncated := end < len(lines)
			var b strings.Builder
			for i, line := range slice {
				fmt.Fprintf(&b, "%6d\t%s\n", start+i+1, line)
			}
			if truncated {
				fmt.Fprintf(&b, "... (truncated; %d lines total)\n", len(lines))
			}
			return coretools.Result{Content: b.String()}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

const writeDescription = `Write a file, creating parent directories as needed. Overwrites the file if it exists.`

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func writeTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("Write", writeDescription,
		func(ctx context.Context, a writeArgs) (coretools.Result, error) {
			path := resolvePath(baseWorkdir, a.FilePath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return coretools.Result{}, fmt.Errorf("write %s: %w", a.FilePath, err)
			}
			if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
				return coretools.Result{}, fmt.Errorf("write %s: %w", a.FilePath, err)
			}
			return coretools.Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.FilePath)}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

const editDescription = `Replace old_string with new_string in a file. By default old_string must appear exactly once; set replace_all to replace every occurrence.`

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func editTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("Edit", editDescription,
		func(ctx context.Context, a editArgs) (coretools.Result, error) {
			path := resolvePath(baseWorkdir, a.FilePath)
			data, err := os.ReadFile(path)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("edit %s: %w", a.FilePath, err)
			}
			content := string(data)
			count := strings.Count(content, a.OldString)
			if count == 0 {
				return coretools.Result{}, fmt.Errorf("old_string not found in %s", a.FilePath)
			}
			if count > 1 && !a.ReplaceAll {
				return coretools.Result{}, fmt.Errorf("old_string appears %d times in %s; use replace_all or make it unique", count, a.FilePath)
			}
			replaced := strings.ReplaceAll(content, a.OldString, a.NewString)
			if err := os.WriteFile(path, []byte(replaced), 0o644); err != nil {
				return coretools.Result{}, fmt.Errorf("edit %s: %w", a.FilePath, err)
			}
			note := "1 occurrence"
			if a.ReplaceAll && count > 1 {
				note = fmt.Sprintf("%d occurrences", count)
			}
			return coretools.Result{Content: fmt.Sprintf("Edited %s (%s)", a.FilePath, note)}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// resolvePath resolves a tool-supplied path against the harness working
// directory.
func resolvePath(baseWorkdir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(baseWorkdir, path)
}
