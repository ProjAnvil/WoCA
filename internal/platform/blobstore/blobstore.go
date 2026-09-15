// Package blobstore defines the object-storage interface used by business
// code plus a local filesystem implementation. In monolith mode the local
// implementation maps keys to files under a root directory; remote
// implementations (e.g. S3, NATS object store) live in separate packages.
package blobstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrNotFound is returned when a key does not exist in the store.
var ErrNotFound = errors.New("blobstore: not found")

// ErrInvalidKey is returned when a key is empty, absolute, or escapes the
// store root via "..".
var ErrInvalidKey = errors.New("blobstore: invalid key")

// Store is the object-storage interface. Keys are slash-separated relative
// paths such as "{tenant}/{session}/transcript.jsonl".
type Store interface {
	// Put stores data under key, creating parent directories as needed.
	Put(ctx context.Context, key string, data []byte) error
	// Get returns the data stored under key, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes key, returning ErrNotFound if it does not exist.
	Delete(ctx context.Context, key string) error
	// List returns the sorted keys under prefix; an empty prefix lists all.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Local is a Store backed by a local directory. It is safe for concurrent
// use by multiple goroutines.
type Local struct {
	root string
	mu   sync.Mutex
}

// NewLocal creates a Local store rooted at root, creating the directory if
// needed.
func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Local{root: root}, nil
}

// resolve validates key and returns the filesystem path under root.
func (l *Local) resolve(key string) (string, error) {
	clean := path.Clean(key)
	if clean == "." || clean == "" || path.IsAbs(key) || strings.HasPrefix(clean, "..") {
		return "", ErrInvalidKey
	}
	return filepath.Join(l.root, filepath.FromSlash(clean)), nil
}

// Put implements Store.
func (l *Local) Put(_ context.Context, key string, data []byte) error {
	p, err := l.resolve(key)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Get implements Store.
func (l *Local) Get(_ context.Context, key string) ([]byte, error) {
	p, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// Delete implements Store.
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.resolve(key)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// List implements Store.
func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	if _, err := l.resolve(prefix); prefix != "" && err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var keys []string
	err := filepath.WalkDir(l.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(keys)
	return keys, nil
}
