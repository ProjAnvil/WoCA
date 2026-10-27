package executor

// Workspace storage for the EE sandbox plane (spec §5.3): host-side
// directories keyed by workspace ID. There is no transfer surface anymore —
// woca does not move files between owners. The sandbox bind-mounts its
// workspace directory and works on it in place, so this file owns exactly
// two things: mapping a validated workspace ID to its directory, and the
// channel checks on the one remaining write path onto the volume, the
// user-initiated manual archive import (PUT /sandbox/{id}/archive):
// entries must stay inside the workspace, symlinks may not escape it, and
// the uncompressed payload is bounded by the store's cap (settings key
// sandbox-archive-max-bytes).

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// sandboxWorkspacePath is the container-side mount point of the workspace
// directory and the path the archive endpoints address.
const sandboxWorkspacePath = "/workspace"

// ErrWorkspaceTooLarge marks an archive crossing the store's maxBytes cap.
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
// out per-ID directories (which the backend bind-mounts as the sandbox's
// /workspace) and bounds the manual archive import channel.
type WorkspaceStore struct {
	root     string
	maxBytes int64
}

// NewWorkspaceStore returns a store rooted at root. maxBytes bounds the
// uncompressed tar payload of a manual archive import; a non-positive cap
// rejects every non-empty archive (fail closed, like the worker's former
// archive cap).
func NewWorkspaceStore(root string, maxBytes int64) *WorkspaceStore {
	return &WorkspaceStore{root: root, maxBytes: maxBytes}
}

// Root returns the store's root directory.
func (s *WorkspaceStore) Root() string { return s.root }

// MaxBytes reports the store's archive payload cap.
func (s *WorkspaceStore) MaxBytes() int64 { return s.maxBytes }

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

// --- manual archive import channel checks -------------------------------------
//
// GuardImport is the streaming validation layer between the user-supplied
// archive body and CopyIn (spec §5.3: the manual import is the only direct
// write onto the volume, so it is policed here).

// GuardImport wraps a user-supplied archive body (tar or tar.gz) in a
// reader that yields a sanitized plain-tar stream: every entry is renamed
// to a clean workspace-relative path, absolute names and ".." traversal
// are rejected, symlink targets may not resolve outside the workspace, and
// the uncompressed payload is capped at the store's maxBytes. A violation
// or an over-cap stream surfaces as the reader's error (wrapped by
// CopyIn), so callers map it from the CopyIn call.
func (s *WorkspaceStore) GuardImport(r io.Reader) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		_ = pw.CloseWithError(rewriteValidatedArchive(pw, r, s.maxBytes))
	}()
	return pr
}

// rewriteValidatedArchive reads an archive stream, validates every entry,
// and re-emits a sanitized plain tar into w. Regular files, directories
// and in-root symlinks pass through (a manual import may carry .git — it
// is the user's own archive, spec §5.3); everything else (hardlinks,
// devices, fifos) is dropped.
func rewriteValidatedArchive(w io.Writer, r io.Reader, maxBytes int64) error {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = gz.Close() }() // read-side: nothing actionable
		src = gz
	}

	tr := tar.NewReader(&countingReader{r: src, max: maxBytes})
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrWorkspaceTooLarge) {
				return ErrWorkspaceTooLarge
			}
			return fmt.Errorf("tar read: %w", err)
		}
		name, err := archiveEntryName(hdr.Name, path.Base(sandboxWorkspacePath))
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		hdr.Name = name
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeReg:
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if hdr.Typeflag == tar.TypeReg {
				if _, err := io.Copy(tw, tr); err != nil {
					if errors.Is(err, ErrWorkspaceTooLarge) {
						return ErrWorkspaceTooLarge
					}
					return fmt.Errorf("tar copy %s: %w", name, err)
				}
			}
		case tar.TypeSymlink:
			if err := checkLinkInRoot(name, hdr.Linkname); err != nil {
				return err
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		default:
			// Hardlinks, devices, fifos and pax headers are not part of a
			// workspace import; drop them.
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return nil
}

// archiveEntryName normalizes a tar entry name to a slash path relative
// to the workspace root, stripping the archive's top-level "workspace/"
// prefix when present (CopyOut naming). Absolute names and names escaping
// via ".." are rejected.
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
// outside the workspace root: such a link would turn later writes into
// arbitrary-path writes on whatever host the /workspace mount lands on.
// The check resolves one hop against the link's own directory; a link
// chain escaping through a second in-root link is not tracked (matching
// the archive policy of the former transfer plumbing).
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

// --- byte caps ------------------------------------------------------------

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
