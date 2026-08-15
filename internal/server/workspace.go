package server

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	workspaceMaxDepth   = 6
	workspaceMaxEntries = 200
	workspaceMaxContent = 64 * 1024
	// workspaceMaxFilePreview bounds on-demand file previews served to the
	// right sidebar (larger than the inline markdown cap so real source files
	// render fully).
	workspaceMaxFilePreview = 256 * 1024
)

var (
	errNotPreviewable = errors.New("file type is not previewable")
	errFileTooLarge   = errors.New("file is too large to preview")
	errNotFound       = errors.New("file not found")
)

// workspaceExcluded are directory names skipped when walking the workdir.
var workspaceExcluded = map[string]bool{
	".git": true, "node_modules": true, "bin": true, "dist": true,
	"dist-demo": true, ".woca": true, "vendor": true, "build": true,
	".next": true, ".cache": true, "coverage": true, "__pycache__": true,
}

// markdownExts are file extensions previewable as Markdown in the UI.
var markdownExts = map[string]bool{
	".md": true, ".mdx": true, ".markdown": true,
}

// workspaceNode mirrors the frontend WorkspaceFile JSON shape.
type workspaceNode struct {
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Kind     string           `json:"kind"`
	Children []*workspaceNode `json:"children,omitempty"`
	Content  string           `json:"content,omitempty"`
}

// buildWorkspaceTree walks root (the agent workdir) into a bounded file tree.
// Markdown files include their content for inline preview; other files are
// listed without content.
func buildWorkspaceTree(root string) *workspaceNode {
	if root == "" {
		return nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	return walkWorkspace(abs, abs, 0)
}

func walkWorkspace(root, dir string, depth int) *workspaceNode {
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	node := &workspaceNode{
		Name: info.Name(),
		Path: nodePath(root, dir),
		Kind: "dir",
	}
	if depth >= workspaceMaxDepth {
		return node
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return node
	}
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || workspaceExcluded[name] {
			continue
		}
		if len(node.Children) >= workspaceMaxEntries {
			break
		}
		p := filepath.Join(dir, name)
		if e.IsDir() {
			if child := walkWorkspace(root, p, depth+1); child != nil {
				node.Children = append(node.Children, child)
			}
			continue
		}
		f := &workspaceNode{Name: name, Path: nodePath(root, p), Kind: "file"}
		if markdownExts[strings.ToLower(filepath.Ext(name))] {
			if b, err := os.ReadFile(p); err == nil && len(b) <= workspaceMaxContent {
				f.Content = string(b)
			}
		}
		node.Children = append(node.Children, f)
	}
	return node
}

func nodePath(root, p string) string {
	if p == root {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

// previewableExts are extensions the right-sidebar preview will render (with
// syntax highlighting on the frontend). Everything else 415s.
var previewableExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".vue": true, ".svelte": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".md": true, ".mdx": true, ".markdown": true,
	".txt": true, ".py": true, ".rs": true, ".rb": true, ".java": true,
	".kt": true, ".swift": true, ".c": true, ".h": true, ".cpp": true,
	".hpp": true, ".cc": true, ".cs": true, ".php": true, ".css": true,
	".scss": true, ".html": true, ".htm": true, ".xml": true, ".sql": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".lua": true,
	".proto": true, ".graphql": true, ".env": true, ".mod": true, ".sum": true,
	".lock": true, ".Makefile": true, ".mk": true, ".ini": true, ".cfg": true,
	".conf": true, ".diff": true, ".patch": true, ".dockerfile": true,
}

// isPreviewable reports whether name's extension allows a text preview.
func isPreviewable(name string) bool {
	if strings.HasPrefix(name, ".") && !strings.Contains(strings.TrimPrefix(name, "."), ".") {
		// Dotfiles like .gitignore / .env-preview have no ext to check; allow
		// a small set by whole name.
		switch strings.ToLower(name) {
		case ".gitignore", ".gitattributes", ".env", ".editorconfig", ".npmrc", ".nvmrc":
			return true
		}
		return false
	}
	return previewableExts[strings.ToLower(filepath.Ext(name))]
}

// readWorkspaceFile loads one file (relative to the workdir root) for preview.
// It rejects path traversal, non-previewable types, binary content and
// oversized files.
func readWorkspaceFile(root, rel string) (*workspaceNode, error) {
	if root == "" || strings.TrimSpace(rel) == "" {
		return nil, errNotFound
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, errNotFound
	}
	p := filepath.Join(absRoot, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, absRoot+string(filepath.Separator)) {
		return nil, errNotFound
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return nil, errNotFound
	}
	if !isPreviewable(info.Name()) {
		return nil, errNotPreviewable
	}
	if info.Size() > workspaceMaxFilePreview {
		return nil, errFileTooLarge
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, errNotFound
	}
	if !utf8.Valid(b) {
		return nil, errNotPreviewable
	}
	return &workspaceNode{
		Name:    info.Name(),
		Path:    filepath.ToSlash(rel),
		Kind:    "file",
		Content: string(b),
	}, nil
}
