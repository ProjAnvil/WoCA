package executor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDirLock_SecondLockIsBusy(t *testing.T) {
	d := t.TempDir()
	l, err := LockDir(d)
	if err != nil {
		t.Fatalf("LockDir(%s): %v", d, err)
	}
	defer func() { _ = l.Close() }()

	if _, err := LockDir(d); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("second LockDir: want ErrWorkspaceBusy, got %v", err)
	}
}

func TestDirLock_CloseReleasesAndIsIdempotent(t *testing.T) {
	d := t.TempDir()
	l, err := LockDir(d)
	if err != nil {
		t.Fatalf("LockDir(%s): %v", d, err)
	}
	if got := l.Path(); got != d {
		t.Errorf("Path() = %q, want %q", got, d)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}

	// The released directory must be lockable again immediately.
	l2, err := LockDir(d)
	if err != nil {
		t.Fatalf("LockDir after Close: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDirLock_IndependentDirectoriesDoNotContend(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	la, err := LockDir(a)
	if err != nil {
		t.Fatalf("LockDir(a): %v", err)
	}
	defer func() { _ = la.Close() }()
	lb, err := LockDir(b)
	if err != nil {
		t.Fatalf("LockDir(b) while a locked: %v", err)
	}
	if err := lb.Close(); err != nil {
		t.Fatalf("Close(b): %v", err)
	}
}

func TestDirLock_RequiresExistingDirectory(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
	}{
		{
			name: "missing dir",
			path: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nope")
			},
		},
		{
			name: "regular file",
			path: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
				return p
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.path(t)
			l, err := LockDir(p)
			if err == nil {
				_ = l.Close()
				t.Fatalf("LockDir(%s) succeeded, want error", p)
			}
			if errors.Is(err, ErrWorkspaceBusy) {
				t.Fatalf("LockDir(%s): unexpected ErrWorkspaceBusy", p)
			}
		})
	}
}

// TestLockDir_IndependentOfContents pins the reason the lock is taken on
// the directory fd itself rather than a lock file inside it: locking must
// neither depend on nor disturb the directory's contents, so a workspace
// directory can be locked and tarred without picking up a lock file.
func TestDirLock_IndependentOfContents(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	before := listDirNames(t, d)

	l, err := LockDir(d)
	if err != nil {
		t.Fatalf("LockDir: %v", err)
	}
	if after := listDirNames(t, d); !slices.Equal(before, after) {
		t.Fatalf("locking changed directory contents: before %v, after %v", before, after)
	}

	// Mutating contents under the lock, then unlocking and relocking, must
	// keep working: the lock does not depend on the tree it protects.
	if err := os.WriteFile(filepath.Join(d, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2, err := LockDir(d)
	if err != nil {
		t.Fatalf("LockDir after content change: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
