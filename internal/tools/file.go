package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

const readDescription = `Read a UTF-8 text file and return its content with line numbers. Use offset/limit to read a slice. Truncates very large files with a note.

Reading an image file (png/jpg/jpeg/gif/webp) attaches it to the tool result as an image block the model can see — use it for screenshots, diagrams, and photos; do not read images as text.`

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

func readTool(d *Dir, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("Read", readDescription,
		func(ctx context.Context, a readArgs) (coretools.Result, error) {
			path := resolvePath(d.For(ctx), a.FilePath)
			if IsImagePath(a.FilePath) {
				data, err := execFor(ctx).ReadFile(ctx, path)
				if err != nil {
					return coretools.Result{Content: fmt.Sprintf("Error: %s is not readable as an image: %v", a.FilePath, err)}, nil
				}
				p, err := ProcessImageData(data)
				if err != nil {
					return coretools.Result{Content: fmt.Sprintf("Error: %s is not readable as an image: %v", a.FilePath, err)}, nil
				}
				content, meta := FormatImageResult(a.FilePath, p)
				return coretools.Result{Content: content, Metadata: meta}, nil
			}
			data, err := execFor(ctx).ReadFile(ctx, path)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("read %s: %w", a.FilePath, err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

			start := max(a.Offset, 0)
			end := len(lines)
			if a.Limit > 0 {
				end = min(start+a.Limit, len(lines))
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
		}))
	return t
}

const writeDescription = `Write a file, creating parent directories as needed. Overwrites the file if it exists.`

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func writeTool(d *Dir, fh *FileHistory, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("Write", writeDescription,
		func(ctx context.Context, a writeArgs) (coretools.Result, error) {
			path := resolvePath(d.For(ctx), a.FilePath)
			x := execFor(ctx)
			before, _ := x.ReadFile(ctx, path) // nil when absent: creation
			if err := x.MkdirAll(ctx, filepath.Dir(path), 0o755); err != nil {
				return coretools.Result{}, fmt.Errorf("write %s: %w", a.FilePath, err)
			}
			if err := x.WriteFile(ctx, path, []byte(a.Content), 0o644); err != nil {
				return coretools.Result{}, fmt.Errorf("write %s: %w", a.FilePath, err)
			}
			fh.Record(ctx, path, before, []byte(a.Content))
			return coretools.Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.FilePath)}, nil
		}))
	return t
}

const editDescription = `Replace old_string with new_string in a file. By default old_string must appear exactly once; set replace_all to replace every occurrence.`

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func editTool(d *Dir, fh *FileHistory, execFor func(context.Context) executor.Executor) coretools.Tool {
	t := MustTool(coretools.FromFunc("Edit", editDescription,
		func(ctx context.Context, a editArgs) (coretools.Result, error) {
			path := resolvePath(d.For(ctx), a.FilePath)
			x := execFor(ctx)
			data, err := x.ReadFile(ctx, path)
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
			if err := x.WriteFile(ctx, path, []byte(replaced), 0o644); err != nil {
				return coretools.Result{}, fmt.Errorf("edit %s: %w", a.FilePath, err)
			}
			fh.Record(ctx, path, data, []byte(replaced))
			note := "1 occurrence"
			if a.ReplaceAll && count > 1 {
				note = fmt.Sprintf("%d occurrences", count)
			}
			return coretools.Result{Content: fmt.Sprintf("Edited %s (%s)", a.FilePath, note)}, nil
		}))
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
