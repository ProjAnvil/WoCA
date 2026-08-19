package local

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/platform/cache"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/runqueue"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

func TestNewAssemblesAllComponents(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if s.Queue == nil {
		t.Error("Queue is nil")
	}
	if s.Bus == nil {
		t.Error("Bus is nil")
	}
	if s.Cache == nil {
		t.Error("Cache is nil")
	}
	if s.Exec == nil {
		t.Error("Exec is nil")
	}
	if s.Blobs == nil {
		t.Error("Blobs is nil")
	}
	if s.Auth == nil {
		t.Error("Auth is nil")
	}
}

func TestSetImplementsInterfaces(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	var _ runqueue.Queue = s.Queue
	var _ eventbus.Bus = s.Bus
	var _ cache.Cache = s.Cache
	var _ executor.Executor = s.Exec
	var _ blobstore.Store = s.Blobs
	var _ authn.Provider = s.Auth
}

func TestSetComponentsWork(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	tc, err := s.Auth.Authenticate(ctx, "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tc.TenantID != tenancy.Local().TenantID {
		t.Errorf("tenant = %q, want %q", tc.TenantID, tenancy.Local().TenantID)
	}

	if err := s.Blobs.Put(ctx, "a/b.txt", []byte("hi")); err != nil {
		t.Fatalf("Blobs.Put: %v", err)
	}
	data, err := s.Blobs.Get(ctx, "a/b.txt")
	if err != nil {
		t.Fatalf("Blobs.Get: %v", err)
	}
	if string(data) != "hi" {
		t.Errorf("Blobs.Get = %q, want %q", data, "hi")
	}

	if err := s.Cache.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Cache.Set: %v", err)
	}
	if _, ok, err := s.Cache.Get(ctx, "k"); err != nil || !ok {
		t.Errorf("Cache.Get ok=%v err=%v, want ok=true", ok, err)
	}
}

func TestCloseShutsDownComponents(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if err := s.Queue.Enqueue(ctx, runqueue.Run{ID: "r1"}); !errors.Is(err, runqueue.ErrClosed) {
		t.Errorf("Queue.Enqueue err = %v, want ErrClosed", err)
	}
	if _, err := s.Bus.Publish(ctx, "t", eventbus.Event{Type: "x"}); !errors.Is(err, eventbus.ErrClosed) {
		t.Errorf("Bus.Publish err = %v, want ErrClosed", err)
	}
	if _, _, err := s.Cache.Get(ctx, "k"); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Cache.Get err = %v, want ErrClosed", err)
	}
}

func TestNewWithInvalidBlobRoot(t *testing.T) {
	// A regular file as blob root makes blobstore.NewLocal fail.
	dir := t.TempDir()
	f := dir + "/file"
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("New with file as blob root: want error, got nil")
	}
}
