// Package fsb implements the workspace folder browser service (product
// design §4.1): directory-only listing, server-confirmed mkdir, debounced
// change watching, and path validation. Every operation is jailed to the
// configured allowed roots using os.OpenRoot, so ../ traversal and symlink
// escapes cannot leave the jail (string-prefix checks are not trustworthy
// — see FileBrowser CVE-2026-54094). Out-of-jail and non-existent paths
// fail identically so the API does not leak which paths exist.
package fsb

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Sentinel errors mapped by the HTTP layer: ErrForbidden → 403 (uniform for
// out-of-jail and non-existent paths), ErrExists → 409, ErrBadName/ErrNotDir
// → 400.
var (
	ErrForbidden = errors.New("fsb: path is outside the allowed roots")
	ErrExists    = errors.New("fsb: directory already exists")
	ErrBadName   = errors.New("fsb: invalid directory name")
	ErrNotDir    = errors.New("fsb: path is not a directory")
)

// Entry describes one directory in a listing.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Mtime    int64  `json:"mtime"`
	Writable bool   `json:"writable"`
}

// Listing is the response of List: the jail-resolved path, its parent
// (empty at a root), the child directories, and the allowed roots.
type Listing struct {
	Path   string   `json:"path"`
	Parent string   `json:"parent"`
	Dirs   []Entry  `json:"dirs"`
	Roots  []string `json:"roots"`
}

// Service browses directories inside its allowed roots. Permissive mode
// (the loopback posture, product design §4.3) jails to the filesystem root
// instead; constrained mode defaults to the user's home directory.
type Service struct {
	roots      []string
	permissive bool
}

// New builds a service over the given allowed roots. Empty roots mean the
// default (home directory) in constrained mode; permissive widens the jail
// to the whole filesystem (loopback, trust-the-machine posture).
func New(roots []string, permissive bool) *Service {
	s := &Service{permissive: permissive}
	if permissive {
		s.roots = []string{string(filepath.Separator)}
		return s
	}
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			s.roots = append(s.roots, resolveExisting(filepath.Clean(abs)))
		}
	}
	if len(s.roots) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			s.roots = []string{home}
		}
	}
	return s
}

// Roots returns the allowed roots (the listing shown for an empty path).
func (s *Service) Roots() []string { return append([]string(nil), s.roots...) }

// jail maps an absolute path onto (root, relative) when the path sits
// inside one of the allowed roots; otherwise it returns ErrForbidden.
func (s *Service) jail(path string) (*os.Root, string, error) {
	path = resolveExisting(filepath.Clean(path))
	if !filepath.IsAbs(path) {
		return nil, "", ErrForbidden
	}
	sep := string(filepath.Separator)
	for _, root := range s.roots {
		if path == root {
			r, err := os.OpenRoot(root)
			if err != nil {
				return nil, "", ErrForbidden
			}
			return r, "", nil
		}
		var rel string
		switch {
		case root == sep: // permissive jail: the whole filesystem
			rel = strings.TrimPrefix(path, sep)
		case strings.HasPrefix(path, root+sep):
			rel = strings.TrimPrefix(path, root+sep)
		default:
			continue
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, "", ErrForbidden
		}
		return r, rel, nil
	}
	return nil, "", ErrForbidden
}

// dotIfEmpty maps the jail-relative path of a root itself onto "." —
// *os.Root rejects empty names.
func dotIfEmpty(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// resolveExisting resolves symlinks in the longest existing prefix of p,
// leaving the missing tail untouched (callers validate paths that may not
// exist yet). macOS temp dirs sit behind /var → /private/var, so raw
// prefix comparison would misjudge otherwise.
func resolveExisting(p string) string {
	rest, suffix := p, ""
	for {
		if rp, err := filepath.EvalSymlinks(rest); err == nil {
			if suffix == "" {
				return rp
			}
			return filepath.Join(rp, suffix)
		}
		parent := filepath.Dir(rest)
		if parent == rest {
			return p
		}
		suffix = filepath.Join(filepath.Base(rest), suffix)
		rest = parent
	}
}

// List returns the child directories of path. An empty path lists the
// allowed roots themselves (no parent, no children).
func (s *Service) List(path string) (Listing, error) {
	if strings.TrimSpace(path) == "" {
		out := Listing{Roots: s.Roots()}
		for _, r := range s.roots {
			if st, err := os.Stat(r); err == nil && st.IsDir() {
				out.Dirs = append(out.Dirs, Entry{
					Name: filepath.Base(r), Path: r,
					Mtime: st.ModTime().UnixMilli(), Writable: probeWritable(r),
				})
			}
		}
		return out, nil
	}

	resolved := resolveExisting(filepath.Clean(path))
	root, rel, err := s.jail(resolved)
	if err != nil {
		return Listing{}, err
	}
	defer func() { _ = root.Close() }()
	// OpenRoot rejects symlink escapes at open time; both escape and
	// non-existence surface as ErrForbidden to avoid leaking paths.
	dirf, err := root.Open(dotIfEmpty(rel))
	if err != nil {
		return Listing{}, ErrForbidden
	}
	defer func() { _ = dirf.Close() }()
	entries, err := dirf.ReadDir(-1)
	if err != nil {
		return Listing{}, ErrForbidden
	}
	out := Listing{Path: resolved, Roots: s.Roots()}
	out.Parent = parentOf(resolved, s.roots)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Ditch symlinked dirs that point outside the jail: stat through
		// the root so escapes fail instead of resolving.
		full := filepath.Join(resolved, e.Name())
		if _, err := root.Stat(filepath.Join(rel, e.Name())); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out.Dirs = append(out.Dirs, Entry{
			Name:     e.Name(),
			Path:     full,
			Mtime:    info.ModTime().UnixMilli(),
			Writable: probeWritable(full),
		})
	}
	slices.SortFunc(out.Dirs, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Mkdir creates name inside parent (server-confirmed, no optimistic
// client update) and returns the created path.
func (s *Service) Mkdir(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) ||
		len(name) > 255 {
		return "", ErrBadName
	}
	root, rel, err := s.jail(parent)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if st, err := root.Stat(dotIfEmpty(rel)); err != nil || !st.IsDir() {
		return "", ErrNotDir
	}
	child := filepath.Join(rel, name)
	if _, err := root.Stat(child); err == nil {
		return "", ErrExists
	}
	if err := root.Mkdir(child, 0o755); err != nil {
		if os.IsExist(err) {
			return "", ErrExists
		}
		return "", fmt.Errorf("fsb: mkdir: %w", err)
	}
	return filepath.Join(filepath.Clean(parent), name), nil
}

// Validate reports whether path exists, is a directory, and is writable.
// Out-of-jail paths report exists=false (same shape as missing paths; the
// caller decides how much to reveal).
func (s *Service) Validate(path string) (exists, isDir, isWritable bool) {
	root, rel, err := s.jail(path)
	if err != nil {
		return false, false, false
	}
	defer func() { _ = root.Close() }()
	st, err := root.Stat(dotIfEmpty(rel))
	if err != nil {
		return false, false, false
	}
	return true, st.IsDir(), probeWritable(path)
}

// Watch streams a change signal for path's direct children until ctx is
// done, debounced: mutations inside one window collapse into one event.
// Polling keeps the dependency set at zero and stays correct on network
// filesystems where inotify-class APIs misbehave.
func (s *Service) Watch(ctx context.Context, path string) (<-chan struct{}, error) {
	if _, err := s.List(path); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, ErrNotDir
	}
	out := make(chan struct{}, 1)
	last := s.signature(path)
	go func() {
		defer close(out)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				sig := s.signature(path)
				if sig == last {
					continue
				}
				last = sig
				// Debounce: coalesce bursts within 300ms into one event.
				time.Sleep(300 * time.Millisecond)
				last = s.signature(path)
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, nil
}

// signature fingerprints a directory's direct children (name+mtime) so the
// watcher can detect additions, removals, and renames.
func (s *Service) signature(path string) string {
	root, rel, err := s.jail(path)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	dirf, err := root.Open(dotIfEmpty(rel))
	if err != nil {
		return ""
	}
	defer func() { _ = dirf.Close() }()
	entries, err := dirf.ReadDir(-1)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mtime := ""
		if info, err := e.Info(); err == nil {
			mtime = info.ModTime().Format(time.RFC3339Nano)
		}
		b.WriteString(e.Name())
		b.WriteByte('|')
		b.WriteString(mtime)
		b.WriteByte(';')
	}
	return b.String()
}

// parentOf returns the visible parent of path: empty at an allowed root
// (browsing stops at the jail edge), the ordinary parent elsewhere.
func parentOf(path string, roots []string) string {
	for _, r := range roots {
		if path == r {
			return ""
		}
	}
	return filepath.Dir(path)
}

// probeWritable reports whether the current user may write to path. The
// probe reflects real ACLs; failures conservatively report false. The
// implementation is platform-split (access_unix.go / access_windows.go).
func probeWritable(path string) bool { return accessWritable(path) }
