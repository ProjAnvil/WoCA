package server

import (
	"cmp"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	dirRank := func(e os.DirEntry) int {
		if e.IsDir() {
			return 0
		}
		return 1
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return cmp.Or(
			cmp.Compare(dirRank(a), dirRank(b)),
			cmp.Compare(a.Name(), b.Name()),
		)
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

// --- EE volume tree/preview (os.Root jail) ---
//
// The personal-edition walk above trusts the local user's filesystem:
// lexical paths, symlinks followed freely. Under the sandbox plane the
// workspace lives in the executor volume and the client is a remote
// tenant, so every resolution goes through an os.Root opened on the
// workspace directory — the same openat-grade containment the fsb folder
// browser uses (/api/filesystem): a symlink leading out of the workspace
// is refused by the jail, never by a string prefix.

// buildVolumeTree walks the workspace opened as root into the same bounded
// tree shape as buildWorkspaceTree. rootName is the resolved workspace
// directory's base (the root node's name/path).
func buildVolumeTree(root *os.Root, rootName string) *workspaceNode {
	return walkVolumeTree(root, ".", rootName, 0)
}

func walkVolumeTree(root *os.Root, dir, rootName string, depth int) *workspaceNode {
	info, err := root.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	node := &workspaceNode{
		Name: volumeNodeName(dir, rootName),
		Path: volumeNodeName(dir, rootName),
		Kind: "dir",
	}
	if depth >= workspaceMaxDepth {
		return node
	}
	f, err := root.Open(dir)
	if err != nil {
		return node
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return node
	}
	dirRank := func(e os.DirEntry) int {
		if e.IsDir() {
			return 0
		}
		return 1
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return cmp.Or(
			cmp.Compare(dirRank(a), dirRank(b)),
			cmp.Compare(a.Name(), b.Name()),
		)
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
		// Stat through the root follows in-volume links but refuses
		// escapes (and broken links): skip what the jail will not resolve.
		st, err := root.Stat(p)
		if err != nil {
			continue
		}
		if st.IsDir() {
			if child := walkVolumeTree(root, p, rootName, depth+1); child != nil {
				node.Children = append(node.Children, child)
			}
			continue
		}
		fn := &workspaceNode{Name: name, Path: filepath.ToSlash(p), Kind: "file"}
		if markdownExts[strings.ToLower(filepath.Ext(name))] {
			if b, err := readVolumeFile(root, p, workspaceMaxContent); err == nil && len(b) <= workspaceMaxContent {
				fn.Content = string(b)
			}
		}
		node.Children = append(node.Children, fn)
	}
	return node
}

// volumeNodeName is the display name of a root-relative directory ("."
// maps to the workspace's base name).
func volumeNodeName(dir, rootName string) string {
	if dir == "." {
		return rootName
	}
	return filepath.Base(dir)
}

// readVolumeFile reads one root-relative file through the jail, bounded at
// max bytes (max+1 read so an oversized file is detectable by length).
func readVolumeFile(root *os.Root, rel string, max int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

// readVolumeWorkspaceFile is the EE preview: one file (relative to the
// workspace) loaded through the os.Root jail — traversal and escaping
// symlinks fail as not-found (no path oracle), the type/size/UTF-8 rules
// match the personal path.
func readVolumeWorkspaceFile(root *os.Root, rel string) (*workspaceNode, error) {
	if strings.TrimSpace(rel) == "" {
		return nil, errNotFound
	}
	p := filepath.FromSlash(rel)
	info, err := root.Stat(p)
	if err != nil || info.IsDir() {
		return nil, errNotFound
	}
	if !isPreviewable(info.Name()) {
		return nil, errNotPreviewable
	}
	if info.Size() > workspaceMaxFilePreview {
		return nil, errFileTooLarge
	}
	b, err := readVolumeFile(root, p, workspaceMaxFilePreview)
	if err != nil || len(b) > workspaceMaxFilePreview {
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
	if rest, ok := strings.CutPrefix(name, "."); ok && !strings.Contains(rest, ".") {
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
