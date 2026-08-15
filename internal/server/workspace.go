package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	workspaceMaxDepth   = 4
	workspaceMaxEntries = 200
	workspaceMaxContent = 64 * 1024
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
