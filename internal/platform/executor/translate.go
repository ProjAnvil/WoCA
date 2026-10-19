package executor

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Translate wraps an Executor and remaps host workspace paths to the
// sandbox root (and results back), per the contract's /workspace rule
// (design §10.1 C-1): tools operate on host-absolute paths under the
// run's workspace, while the sandbox is an independent filesystem whose
// working root is SandboxRoot (the contract default /workspace). Requests
// carry sandbox paths out; path-shaped results (glob matches, grep match
// paths) map back so callers keep seeing host paths. Paths outside
// HostRoot pass through unchanged — the backend's error for them is the
// faithful signal. Empty roots disable mapping (identity wrapper).
type Translate struct {
	Inner       Executor
	HostRoot    string // absolute host workspace path
	SandboxRoot string // contract default: /workspace
}

// compile-time interface check.
var _ Executor = (*Translate)(nil)

// IsSandbox delegates to the wrapped executor so executor.IsSandbox
// keeps reporting the truth through the translation layer.
func (t *Translate) IsSandbox() bool { return IsSandbox(t.Inner) }

// toSandbox maps one host path under HostRoot to its SandboxRoot form.
// Sandbox paths are slash-separated regardless of the host OS (the
// contract's filesystem is remote).
func (t *Translate) toSandbox(path string) string {
	if t.HostRoot == "" || t.SandboxRoot == "" || path == "" {
		return path
	}
	host := filepath.ToSlash(filepath.Clean(t.HostRoot))
	root := strings.TrimRight(filepath.ToSlash(filepath.Clean(t.SandboxRoot)), "/")
	p := filepath.ToSlash(filepath.Clean(path))
	if p == host {
		return root
	}
	if strings.HasPrefix(p, host+"/") {
		return root + "/" + strings.TrimPrefix(p, host+"/")
	}
	return path
}

// toSandboxPattern maps a glob pattern by replacing a literal HostRoot
// prefix (Clean would destroy the wildcards, so this is a raw prefix
// swap of the cleaned root).
func (t *Translate) toSandboxPattern(pattern string) string {
	if t.HostRoot == "" || t.SandboxRoot == "" || pattern == "" {
		return pattern
	}
	host := strings.TrimRight(filepath.ToSlash(filepath.Clean(t.HostRoot)), "/")
	root := strings.TrimRight(filepath.ToSlash(filepath.Clean(t.SandboxRoot)), "/")
	if pattern == host {
		return root
	}
	if strings.HasPrefix(pattern, host+"/") {
		return root + "/" + strings.TrimPrefix(pattern, host+"/")
	}
	return pattern
}

// toHost maps one sandbox path under SandboxRoot back to its host form.
func (t *Translate) toHost(path string) string {
	if t.HostRoot == "" || t.SandboxRoot == "" || path == "" {
		return path
	}
	root := strings.TrimRight(filepath.ToSlash(filepath.Clean(t.SandboxRoot)), "/")
	p := filepath.ToSlash(filepath.Clean(path))
	if p == root {
		return filepath.Clean(t.HostRoot)
	}
	if strings.HasPrefix(p, root+"/") {
		return filepath.Join(t.HostRoot, filepath.FromSlash(strings.TrimPrefix(p, root+"/")))
	}
	return path
}

// Exec runs a command with Dir translated to the sandbox root.
func (t *Translate) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	req.Dir = t.toSandbox(req.Dir)
	return t.Inner.Exec(ctx, req)
}

// Start launches a long-running command with Dir translated to the
// sandbox root.
func (t *Translate) Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error) {
	req.Dir = t.toSandbox(req.Dir)
	return t.Inner.Start(ctx, req, opts)
}

// ReadFile reads the whole file at the translated path.
func (t *Translate) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return t.Inner.ReadFile(ctx, t.toSandbox(path))
}

// WriteFile writes data at the translated path.
func (t *Translate) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	return t.Inner.WriteFile(ctx, t.toSandbox(path), data, perm)
}

// MkdirAll creates the translated path.
func (t *Translate) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	return t.Inner.MkdirAll(ctx, t.toSandbox(path), perm)
}

// Stat returns the inner file information for the translated path. The
// returned fs.FileInfo carries only the base name (the interface has no
// full-path field), so it is passed through untranslated — Name() is
// path-independent and callers that need a full path join it themselves.
func (t *Translate) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	return t.Inner.Stat(ctx, t.toSandbox(path))
}

// Glob translates the pattern's root prefix and maps matches back to
// host paths.
func (t *Translate) Glob(ctx context.Context, pattern string) ([]string, error) {
	matches, err := t.Inner.Glob(ctx, t.toSandboxPattern(pattern))
	if err != nil {
		return nil, err
	}
	for i, m := range matches {
		matches[i] = t.toHost(m)
	}
	return matches, nil
}

// Grep translates the root and maps each match's path back to host form.
func (t *Translate) Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]GrepMatch, error) {
	matches, err := t.Inner.Grep(ctx, t.toSandbox(root), pattern)
	if err != nil {
		return nil, err
	}
	for i := range matches {
		matches[i].Path = t.toHost(matches[i].Path)
	}
	return matches, nil
}
