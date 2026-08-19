package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Local {
	t.Helper()
	s, err := NewLocal(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := "tenant1/session1/transcript.jsonl"
	data := []byte("hello blob")
	if err := s.Put(ctx, key, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}
	// Mutating the returned slice must not affect the stored copy.
	got[0] = 'X'
	again, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if !bytes.Equal(again, data) {
		t.Fatalf("stored data mutated: %q", again)
	}
}

func TestNestedKeyCreatesParents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := "a/b/c/d.txt"
	if err := s.Put(ctx, key, []byte("deep")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Get(ctx, key); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get(context.Background(), "missing/key")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete err = %v, want ErrNotFound", err)
	}
}

func TestListPrefixAndSort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	keys := []string{
		"t2/s1/b.txt",
		"t1/s2/y.txt",
		"t1/s1/a.txt",
		"t1/s1/sub/c.txt",
	}
	for _, k := range keys {
		if err := s.Put(ctx, k, []byte("x")); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		{"", []string{"t1/s1/a.txt", "t1/s1/sub/c.txt", "t1/s2/y.txt", "t2/s1/b.txt"}},
		{"t1", []string{"t1/s1/a.txt", "t1/s1/sub/c.txt", "t1/s2/y.txt"}},
		{"t1/s1", []string{"t1/s1/a.txt", "t1/s1/sub/c.txt"}},
		{"t1/s1/sub/c.txt", []string{"t1/s1/sub/c.txt"}},
		{"nope", nil},
	}
	for _, tc := range cases {
		got, err := s.List(ctx, tc.prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", tc.prefix, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("List(%q) = %v, want %v", tc.prefix, got, tc.want)
		}
	}
}

func TestInvalidKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bad := []string{"../x", "/abs", "a/../../b", "", ".", ".."}
	for _, op := range []struct {
		name string
		fn   func(string) error
	}{
		{"Put", func(k string) error { return s.Put(ctx, k, []byte("x")) }},
		{"Get", func(k string) error { _, err := s.Get(ctx, k); return err }},
		{"Delete", func(k string) error { return s.Delete(ctx, k) }},
	} {
		for _, k := range bad {
			if err := op.fn(k); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("%s(%q) err = %v, want ErrInvalidKey", op.name, k, err)
			}
		}
	}
	// List with an invalid prefix also fails.
	if _, err := s.List(ctx, "../x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("List(../x) err = %v, want ErrInvalidKey", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("t/s%d/item.txt", i%3)
			for j := 0; j < 20; j++ {
				if err := s.Put(ctx, key, []byte("v")); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				// Keys are shared across goroutines, so a peer may delete
				// between our Put and Get; ErrNotFound is expected there.
				if _, err := s.Get(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("Get: %v", err)
					return
				}
				if _, err := s.List(ctx, "t"); err != nil {
					t.Errorf("List: %v", err)
					return
				}
				if err := s.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestListWalkError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, "locked/file", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// An unreadable subdirectory makes the walk fail.
	dir := filepath.Join(s.root, "locked")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := s.List(ctx, ""); err == nil {
		t.Fatal("List succeeded with an unreadable directory, want error")
	}
}

func TestNewLocalInvalidRoot(t *testing.T) {
	// A regular file cannot be used as the root directory.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(f); err == nil {
		t.Fatal("NewLocal on a file path succeeded, want error")
	}
}

func TestFilesystemErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// "a" is a regular file, so creating parent dirs for "a/b" fails.
	if err := s.Put(ctx, "a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a/b", []byte("x")); err == nil {
		t.Fatal("Put under a file succeeded, want error")
	}
	// Reading a directory key is a non-NotExist error.
	if err := s.Put(ctx, "dir/file", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "dir"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on a directory err = %v, want non-NotExist error", err)
	}
	// Deleting a non-empty directory key is a non-NotExist error.
	if err := s.Delete(ctx, "dir"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete on a directory err = %v, want non-NotExist error", err)
	}
}
