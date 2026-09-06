// Package promptref expands local @path prompt references into bounded context.
package promptref

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxFiles        = 40
	maxFileBytes    = 32 << 10
	maxContentBytes = 256 << 10
	maxCandidates   = 100
)

// Candidate is one filesystem completion candidate. Directories have a
// trailing slash in Path so accepting a directory completion keeps typing
// inside that directory natural.
type Candidate struct {
	Path  string
	IsDir bool
}

// Candidates lists workspace-local paths that begin with prefix. It never
// follows symlinks and skips Git metadata.
func Candidates(prefix, workdir string) []Candidate {
	if filepath.IsAbs(prefix) || containsParentPath(prefix) {
		return nil
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return nil
	}
	prefixed := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(prefix, "./")))
	var candidates []Candidate
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || len(candidates) >= maxCandidates {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			name += "/"
		}
		if strings.HasPrefix(strings.ToLower(name), prefixed) {
			candidates = append(candidates, Candidate{Path: name, IsDir: entry.IsDir()})
		}
		return nil
	})
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].Path < candidates[right].Path })
	return candidates
}

// Expand replaces workspace-local @path references with the referenced file
// content. Mentions that do not resolve to a path are preserved so @agent
// continues to work as a subagent mention.
func Expand(text, workdir string) (string, error) {
	root, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}

	var out strings.Builder
	for index := 0; index < len(text); {
		if text[index] != '@' {
			out.WriteByte(text[index])
			index++
			continue
		}
		end := index + 1
		for end < len(text) && referenceByte(text[end]) {
			end++
		}
		if end == index+1 {
			out.WriteByte(text[index])
			index++
			continue
		}
		reference := text[index+1 : end]
		content, found, err := readFile(root, reference)
		if err != nil {
			return "", err
		}
		if !found {
			out.WriteString(text[index:end])
			index = end
			continue
		}
		out.WriteString(content)
		index = end
	}
	return out.String(), nil
}

func referenceByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_' || value == '-' ||
		value == '.' || value == '/'
}

func readFile(root, reference string) (string, bool, error) {
	path := reference
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read @%s: %w", reference, err)
	}
	if !inside(root, resolved) {
		return "", false, fmt.Errorf("@%s is outside the workspace", reference)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("read @%s: %w", reference, err)
	}
	if info.IsDir() {
		return readDirectory(root, resolved, reference)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("@%s is not a regular file or directory", reference)
	}
	return readTextFile(root, resolved, reference)
}

func readDirectory(root, directory, reference string) (string, bool, error) {
	var paths []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == directory {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		paths = append(paths, path)
		if len(paths) > maxFiles {
			return fmt.Errorf("@%s expands to more than %d files", reference, maxFiles)
		}
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("read @%s: %w", reference, err)
	}
	sort.Strings(paths)
	var out strings.Builder
	for _, path := range paths {
		content, _, err := readTextFile(root, path, reference)
		if err != nil {
			return "", false, err
		}
		if out.Len()+len(content) > maxContentBytes {
			return "", false, fmt.Errorf("@%s expands to more than %d bytes", reference, maxContentBytes)
		}
		out.WriteString(content)
	}
	return out.String(), true, nil
}

func readTextFile(root, path, reference string) (string, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("read @%s: %w", reference, err)
	}
	if info.Size() > maxFileBytes {
		return "", false, fmt.Errorf("@%s exceeds the %d-byte file limit", reference, maxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read @%s: %w", reference, err)
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return "", false, fmt.Errorf("@%s is a binary file", reference)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false, fmt.Errorf("resolve @%s: %w", reference, err)
	}
	return fmt.Sprintf("<woca-file path=%q>\n%s\n</woca-file>\n", filepath.ToSlash(rel), data), true, nil
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func containsParentPath(path string) bool {
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return true
		}
	}
	return false
}
