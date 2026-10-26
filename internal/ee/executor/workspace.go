package executor

// Workspace storage for the EE sandbox plane: host-side directories keyed
// by workspace ID, plus the tar-based synchronization onto and back from a
// sandbox container. The archive rules mirror internal/ee/worker's per-run
// sandbox plumbing (CONTRACT.md §4): .git never travels, nothing may
// escape the workspace root, and both directions are bounded by a size
// cap counted on the uncompressed tar payload — a highly compressible
// workspace cannot slip past the cap via gzip.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// sandboxWorkspacePath is the container-side directory workspaces are
// uploaded into and downloaded from.
const sandboxWorkspacePath = "/workspace"

// ErrWorkspaceTooLarge marks a workspace archive crossing the store's
// maxBytes cap, in either direction.
var ErrWorkspaceTooLarge = errors.New("executor: workspace archive exceeds size cap")

// ValidWorkspaceID reports whether id is a safe workspace directory name:
// non-empty and limited to letters, digits, underscore and dash
// (^[A-Za-z0-9_-]+$). Everything else — separators, dots, spaces, any
// path-building character — is rejected before it reaches the filesystem.
func ValidWorkspaceID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// WorkspaceStore manages workspace directories under one root: it hands
// out per-ID directories and moves their content to and from sandbox
// containers as a bounded tar.gz stream. Callers serialize per-workspace
// access with LockDir (Upload and Sync assume exclusive ownership of the
// directory for their duration).
type WorkspaceStore struct {
	root     string
	maxBytes int64
}

// NewWorkspaceStore returns a store rooted at root. maxBytes bounds the
// uncompressed tar payload of every upload and download; a non-positive
// cap rejects every non-empty archive (fail closed, like the worker's
// archive cap).
func NewWorkspaceStore(root string, maxBytes int64) *WorkspaceStore {
	return &WorkspaceStore{root: root, maxBytes: maxBytes}
}

// Root returns the store's root directory.
func (s *WorkspaceStore) Root() string { return s.root }

// Dir validates id and returns root/<id>, creating it (and the store
// root) when missing.
func (s *WorkspaceStore) Dir(id string) (string, error) {
	if !ValidWorkspaceID(id) {
		return "", fmt.Errorf("workspace: invalid id %q", id)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return "", fmt.Errorf("workspace: root %s: %w", s.root, err)
	}
	dir := filepath.Join(s.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// Upload packs the workspace root/<id> as a tar.gz stream (entries
// workspace-relative, permission bits and symlinks preserved, .git
// skipped) and streams it into the container's /workspace via CopyIn.
// Symlinks whose target is absolute or resolves outside the workspace
// are rejected, as are non-regular files: either would let a workspace
// write reach arbitrary container paths. Crossing maxBytes aborts the
// upload with an error wrapping ErrWorkspaceTooLarge.
func (s *WorkspaceStore) Upload(ctx context.Context, c Container, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("workspace upload %s: nil container", id)
	}
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	// Closing the read side unblocks the packing goroutine if CopyIn
	// returns without draining the stream (e.g. on ctx cancellation).
	defer func() { _ = pr.Close() }()
	go func() {
		err := func() error {
			gw := gzip.NewWriter(pw)
			// Count the uncompressed tar payload, not the gzip stream.
			cw := &countingWriter{w: gw, max: s.maxBytes}
			tw := tar.NewWriter(cw)
			if err := tarWorkspace(tw, dir); err != nil {
				return err
			}
			if err := tw.Close(); err != nil {
				return err
			}
			if err := gw.Close(); err != nil {
				return err
			}
			return nil
		}()
		_ = pw.CloseWithError(err)
	}()

	if err := c.CopyIn(ctx, sandboxWorkspacePath, pr); err != nil {
		if errors.Is(err, ErrWorkspaceTooLarge) {
			return err
		}
		return fmt.Errorf("workspace upload %s: %w", id, err)
	}
	return nil
}

// Sync downloads the container's /workspace via CopyOut, validates it
// under the same rules as Upload, and atomically replaces root/<id> with
// the result: the archive is extracted into a staging directory directly
// under the store root (never inside a workspace, so it cannot leak into
// later uploads) and renamed into place only after the full tree
// validated. Any failure — copy-out, validation, size cap, swap — keeps
// the previous directory intact.
func (s *WorkspaceStore) Sync(ctx context.Context, c Container, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidWorkspaceID(id) {
		return fmt.Errorf("workspace sync: invalid id %q", id)
	}
	if c == nil {
		return fmt.Errorf("workspace sync %s: nil container", id)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("workspace sync %s: root %s: %w", id, s.root, err)
	}

	rc, err := c.CopyOut(ctx, sandboxWorkspacePath)
	if err != nil {
		return fmt.Errorf("workspace sync %s: copy out: %w", id, err)
	}
	defer func() { _ = rc.Close() }()

	staging, err := os.MkdirTemp(s.root, id+".staging-")
	if err != nil {
		return fmt.Errorf("workspace sync %s: staging dir: %w", id, err)
	}
	if err := extractWorkspaceTar(rc, staging, s.maxBytes); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("workspace sync %s: %w", id, err)
	}
	// MkdirTemp creates 0700; workspaces are 0755 via Dir.
	if err := os.Chmod(staging, 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("workspace sync %s: staging chmod: %w", id, err)
	}
	if err := replaceDir(staging, filepath.Join(s.root, id)); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("workspace sync %s: %w", id, err)
	}
	return nil
}

// replaceDir atomically swaps staging in for dir: the current directory,
// when present, is renamed aside (a sibling of staging, outside every
// workspace), staging takes its place, and the old tree is deleted. A
// failure between the two renames restores the old directory, so a
// failed swap never destroys the previous workspace. Callers hold the
// workspace's DirLock, so nothing else observes the intermediate state.
func replaceDir(staging, dir string) error {
	_, statErr := os.Lstat(dir)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		if err := os.Rename(staging, dir); err != nil {
			return fmt.Errorf("install workspace: %w", err)
		}
		return nil
	case statErr != nil:
		return fmt.Errorf("stat current workspace: %w", statErr)
	}

	prev := staging + ".prev"
	if err := os.Rename(dir, prev); err != nil {
		return fmt.Errorf("move current workspace aside: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		if rbErr := os.Rename(prev, dir); rbErr != nil {
			return fmt.Errorf("install workspace: %v (restore failed: %v)", err, rbErr)
		}
		return fmt.Errorf("install workspace: %w", err)
	}
	if err := os.RemoveAll(prev); err != nil {
		return fmt.Errorf("delete replaced workspace: %w", err)
	}
	return nil
}

// --- upload-side packing ----------------------------------------------------

// tarWorkspace walks the workspace and writes one tar entry per file,
// directory, and symlink, skipping .git and rejecting anything that could
// write outside the workspace from inside the container.
func tarWorkspace(tw *tar.Writer, ws string) error {
	return filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(ws, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, serr := d.Info()
		if serr != nil {
			return serr
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = rel
		switch {
		case d.IsDir():
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			if err := checkLinkInRoot(rel, target); err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = target
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, oerr := os.Open(p)
			if oerr != nil {
				return oerr
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// FIFOs, devices and sockets would block or smuggle nodes into
			// the container; a workspace carrying them is rejected outright.
			return fmt.Errorf("unsupported file type %v at %s", info.Mode()&os.ModeType, rel)
		}
		return nil
	})
}

// --- download-side extraction -----------------------------------------------

// extractWorkspaceTar unpacks a (plain or gzipped) tar stream from
// CopyOut(/workspace) into dst. Container.CopyOut names entries under the
// copied path's base name (Docker archive convention), so a leading
// "workspace/" component is stripped; entries already workspace-relative
// also work. Validation mirrors the upload rules plus the worker's
// download hardening: .git is never reintroduced (a hostile backend could
// otherwise plant git hooks in host workspaces), entry names and symlink
// targets must stay inside the workspace, and the uncompressed payload is
// capped at maxBytes.
func extractWorkspaceTar(r io.Reader, dst string, maxBytes int64) error {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = gz.Close() }()
		src = gz
	}

	tr := tar.NewReader(&countingReader{r: src, max: maxBytes})
	top := path.Base(sandboxWorkspacePath)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if errors.Is(err, ErrWorkspaceTooLarge) {
				return ErrWorkspaceTooLarge
			}
			return fmt.Errorf("tar read: %w", err)
		}
		name, err := archiveEntryName(hdr.Name, top)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		// .git is host-owned and never uploaded; keep it out of the
		// extracted tree whatever the archive claims.
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			continue
		}

		target := filepath.Join(dst, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, headerPerm(hdr, 0o755)); err != nil {
				return fmt.Errorf("mkdir %s: %w", name, err)
			}
		case tar.TypeSymlink:
			if err := checkLinkInRoot(name, hdr.Linkname); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir for %s: %w", name, err)
			}
			if err := writeTarFile(tr, target, headerPerm(hdr, 0o644)); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		default:
			// Hardlinks, devices, fifos and pax headers are not part of a
			// workspace round-trip; skip them.
		}
	}
}

// writeTarFile streams one regular entry to disk.
func writeTarFile(tr *tar.Reader, target string, perm os.FileMode) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, tr); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// archiveEntryName normalizes a tar entry name to a slash path relative
// to the workspace root, stripping the archive's top-level "workspace/"
// prefix when present. Absolute names and names escaping via ".." are
// rejected.
func archiveEntryName(name, top string) (string, error) {
	n := path.Clean(filepath.ToSlash(name))
	if n == "." || n == "/" {
		return ".", nil
	}
	if strings.HasPrefix(n, "/") {
		return "", fmt.Errorf("archive entry %q: absolute path", name)
	}
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", fmt.Errorf("archive entry %q: escapes workspace", name)
	}
	if n == top {
		return ".", nil
	}
	if strings.HasPrefix(n, top+"/") {
		n = n[len(top)+1:]
	}
	if n == "" || n == "." {
		return ".", nil
	}
	return n, nil
}

// checkLinkInRoot rejects a symlink whose target is absolute or resolves
// outside the workspace root: inside the container such a link turns
// later writes into arbitrary container-path writes, and on the host it
// would turn Sync's extraction into arbitrary host-path writes. The check
// resolves one hop against the link's own directory; a link chain
// escaping through a second in-root link is not tracked (matching the
// worker's archive policy).
func checkLinkInRoot(name, link string) error {
	l := path.Clean(filepath.ToSlash(link))
	if strings.HasPrefix(l, "/") {
		return fmt.Errorf("symlink %s: absolute target %q", name, link)
	}
	resolved := path.Join(path.Dir(name), l)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink %s: target %q escapes workspace", name, link)
	}
	return nil
}

// headerPerm picks a tar header's permission bits, falling back to def
// when the header carries none.
func headerPerm(hdr *tar.Header, def os.FileMode) os.FileMode {
	if p := hdr.FileInfo().Mode().Perm(); p != 0 {
		return p
	}
	return def
}

// --- byte caps --------------------------------------------------------------

// countingWriter aborts the stream once more than max bytes were written.
type countingWriter struct {
	w      io.Writer
	n, max int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.n > c.max {
		return 0, ErrWorkspaceTooLarge
	}
	return c.w.Write(p)
}

// countingReader aborts the stream once more than max bytes were read.
type countingReader struct {
	r      io.Reader
	n, max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, ErrWorkspaceTooLarge
	}
	return n, err
}
