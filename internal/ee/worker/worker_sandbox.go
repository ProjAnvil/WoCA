package worker

// Per-run sandbox plumbing (design §4.5): workspace-routed runs (EE)
// create the sandbox under the run's workspace and confirm products with
// a workspace sync; workspace-less runs (personal edition / transition)
// move the workspace itself over the provider's archive surface, both
// directions bounded by the sandbox-archive-max-bytes setting.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/providers"
	"github.com/projanvil/woca/internal/runexec"
	"github.com/projanvil/woca/internal/runstate"
)

// archiver is the optional archive surface of the contract family
// (*executor.Sandbox and the cfbridge provider implement it).
type archiver interface {
	PutArchive(ctx context.Context, r io.Reader) error
	GetArchive(ctx context.Context) (io.ReadCloser, error)
}

// workspaceSyncer is the EE workspace surface (CONTRACT.md §3.16): a
// workspace-routed sandbox flushes its /workspace back to the
// executor-side workspace directory, which is how a run confirms its
// products durably in workspace mode.
type workspaceSyncer interface {
	SyncWorkspace(ctx context.Context) error
}

// provisionRunSandbox sets up the per-run sandbox when the materialized
// config carries an enabled backend (design §4.5). On success it rewrites
// *ctx to carry the run executor and returns the product download
// (idempotent; primary=true fails the run on error, primary=false is the
// error-path best-effort recovery) plus the teardown that destroys the
// sandbox. Nil returns mean no backend (or no workspace to upload) — the
// run executes locally, exactly the pre-sandbox behavior. Every failure
// is returned as an error so the caller fails the run: no silent local
// fallback, including on sandbox-table read errors.
//
// workspaceID selects the artifact mode: with a workspace identity on a
// sync-capable backend (contract-http EE) the sandbox is created under
// that workspace and the executor seeds it during create (spec §4) — no
// client-side upload (that would push the same directory twice) — and
// the product confirmation is SyncWorkspace. Without one (personal
// edition / transition, or archive-only backends like cfbridge) the
// client-side archive round trip applies, unchanged.
func (w *Worker) provisionRunSandbox(ctxp *context.Context, workspaceID string, snap runstate.RunSnapshot) (download func(primary bool) error, teardown func(), err error) {
	if w.db == nil || snap.Workspace == "" {
		return nil, nil, nil
	}
	row, err := nacosrt.EnabledSandbox(w.db.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: enabled backend read: %w", err)
	}
	if row == nil {
		return nil, nil, nil
	}
	secret := ""
	if w.secretFor != nil {
		secret = w.secretFor(row.ID)
	}
	x, err := providers.New(providers.Config{
		Type:     row.Type,
		Endpoint: row.Endpoint,
		APIKey:   secret,
		Timeout:  time.Duration(row.TimeoutMs) * time.Millisecond,
		// The run's workspace identity (runstate channel, Task 14): rides
		// the contract-http create body and every request's routing header.
		Workspace: workspaceID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: provision backend %q (%s): %w", row.Name, row.Type, err)
	}
	teardown = func() { _ = closeIfClosable(x) }
	// In workspace mode the host workspace path is the shared-volume mount
	// (scheduler and executor see the same path), so host-path translation
	// keeps working even though the scheduler-side copy is not the source
	// of truth anymore — recorded here so the routing is observable.
	tx := &executor.Translate{Inner: x, HostRoot: snap.Workspace, SandboxRoot: "/workspace"}
	var once sync.Once
	var dlErr error
	if ws, ok := x.(workspaceSyncer); ok && workspaceID != "" {
		download = func(primary bool) error {
			once.Do(func() {
				if serr := ws.SyncWorkspace(*ctxp); serr != nil {
					dlErr = fmt.Errorf("workspace sync: %w", serr)
				}
				if dlErr != nil && !primary {
					// Error path: the run already failed for its own
					// reason; the product confirmation is best-effort and
					// only logged.
					w.log.Warn().Err(dlErr).Msg("sandbox: best-effort workspace sync failed")
					dlErr = nil
				}
			})
			return dlErr
		}
	} else {
		if workspaceID != "" {
			// A workspace-routed run on a backend without the sync surface
			// (cfbridge today): the workspace identity cannot route, so the
			// client-side archive round trip remains the only way to move
			// the workspace. Observable on purpose — this is a degraded
			// mode for EE tenants.
			w.log.Warn().Str("backend", row.Name).Str("type", row.Type).
				Msg("sandbox: backend lacks workspace sync; falling back to client-side archives")
		}
		// The archive surface is path-free (tar entries are
		// workspace-relative, CONTRACT.md §4), so the raw executor — not
		// the Translate wrapper — serves the upload/download.
		if err := uploadWorkspace(*ctxp, x, snap.Workspace, w.maxArchiveBytes); err != nil {
			teardown()
			return nil, nil, fmt.Errorf("sandbox: workspace upload: %w", err)
		}
		download = func(primary bool) error {
			once.Do(func() {
				dlErr = downloadWorkspace(*ctxp, x, snap.Workspace, w.maxArchiveBytes)
				if dlErr != nil && !primary {
					// Error path: the run already failed for its own reason;
					// the product recovery is best-effort and only logged.
					w.log.Warn().Err(dlErr).Msg("sandbox: best-effort product download failed")
					dlErr = nil
				}
			})
			return dlErr
		}
	}
	*ctxp = runexec.WithRunExecutor(*ctxp, tx)
	w.log.Info().Str("backend", row.Name).Str("type", row.Type).Msg("run sandbox provisioned")
	return download, teardown, nil
}

// errArchiveTooLarge marks a workspace archive crossing the configured
// cap (either direction).
var errArchiveTooLarge = errors.New("workspace archive exceeds sandbox-archive-max-bytes")

// closeIfClosable destroys a provider sandbox when it supports it.
func closeIfClosable(x executor.Executor) error {
	if c, ok := x.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// countingWriter aborts the stream once more than max bytes were written.
// It counts the uncompressed tar payload (upstream of gzip), so a highly
// compressible workspace cannot slip past the cap.
type countingWriter struct {
	w      io.Writer
	n, max int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.n > c.max {
		return 0, errArchiveTooLarge
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
		return n, errArchiveTooLarge
	}
	return n, err
}

// uploadWorkspace tars the host workspace (entries relative to the root,
// permission bits and symlinks preserved, .git excluded — CONTRACT.md §4)
// and uploads it into the sandbox. The stream is bounded by maxBytes:
// crossing the cap aborts the upload with errArchiveTooLarge.
func uploadWorkspace(ctx context.Context, x executor.Executor, ws string, maxBytes int64) error {
	arch, ok := x.(archiver)
	if !ok {
		return errors.New("sandbox backend does not support workspace archives")
	}
	pr, pw := io.Pipe()
	go func() {
		err := func() error {
			gw := gzip.NewWriter(pw)
			// Count the uncompressed tar payload (not the gzip stream): a
			// highly compressible workspace must not slip past the cap.
			cw := &countingWriter{w: gw, max: maxBytes}
			tw := tar.NewWriter(cw)
			err := tarWorkspace(tw, ws)
			if err != nil {
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
	if err := arch.PutArchive(ctx, pr); err != nil {
		if errors.Is(err, errArchiveTooLarge) {
			return err
		}
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// tarWorkspace walks the host workspace and writes one tar entry per
// file, directory, and symlink, skipping .git.
func tarWorkspace(tw *tar.Writer, ws string) error {
	return filepath.WalkDir(ws, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(ws, path)
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
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = target
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		default:
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, oerr := os.Open(path)
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
		}
		return nil
	})
}

// downloadWorkspace fetches the sandbox workspace archive and unpacks it
// back into the host workspace (overwriting; the host's .git directory
// stays untouched — it is never uploaded, so the sandbox copy has none).
// The download stream is bounded by maxBytes: crossing the cap aborts
// with errArchiveTooLarge.
func downloadWorkspace(ctx context.Context, x executor.Executor, ws string, maxBytes int64) error {
	arch, ok := x.(archiver)
	if !ok {
		return errors.New("sandbox backend does not support workspace archives")
	}
	rc, err := arch.GetArchive(ctx)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = rc.Close() }()

	zr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	// Bound the uncompressed tar payload (downstream of gunzip): a highly
	// compressible sandbox workspace must not slip past the cap.
	tr := tar.NewReader(&countingReader{r: zr, max: maxBytes})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errArchiveTooLarge) {
				return errArchiveTooLarge
			}
			return fmt.Errorf("download: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			continue
		}
		// .git is host-owned (excluded on upload) and must not be
		// reintroduced by the backend: a hostile backend could otherwise
		// plant git hooks on the host workspace via run products.
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			continue
		}
		target := filepath.Join(ws, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Skip symlinks whose targets escape the workspace: an
			// absolute or ../ link turns later host-side writes into
			// arbitrary-path writes.
			link := filepath.ToSlash(filepath.Clean(hdr.Linkname))
			if strings.HasPrefix(link, "/") || link == ".." || strings.HasPrefix(link, "../") {
				continue
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				if errors.Is(err, errArchiveTooLarge) {
					return errArchiveTooLarge
				}
				return err
			}
			if err := os.WriteFile(target, data, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}
