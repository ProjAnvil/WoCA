package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func TestMemoryBackendCRUD(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	if err := s.Put(ctx, "prefs", "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, "prefs", "theme")
	if err != nil || !ok || string(v) != `"dark"` {
		t.Fatalf("get = %q %v %v", v, ok, err)
	}
	entries, err := s.List(ctx, "prefs", "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("list = %v %v", entries, err)
	}
	if err := s.Delete(ctx, "prefs", "theme"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "prefs", "theme"); ok {
		t.Fatal("delete did not remove the entry")
	}
}

func TestSQLiteBackendRoundTrip(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewSQL(db.DB, string(db.Dialect), db.Path)
	ctx := context.Background()

	if err := s.Put(ctx, "facts", "user-name", "yuhao"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "facts", "user-city", "shanghai"); err != nil {
		t.Fatal(err)
	}
	// Update in place.
	if err := s.Put(ctx, "facts", "user-name", "chen"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, "facts", "user-name")
	if err != nil || !ok || string(v) != `"chen"` {
		t.Fatalf("get = %q %v %v", v, ok, err)
	}

	entries, err := s.List(ctx, "facts", "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("list = %v %v", entries, err)
	}
	if entries[0].Key != "user-city" || entries[1].Key != "user-name" {
		t.Fatalf("list not ordered: %+v", entries)
	}

	prefixed, err := s.List(ctx, "facts", "user-")
	if err != nil || len(prefixed) != 2 {
		t.Fatalf("prefix list = %v %v", prefixed, err)
	}

	// Cross-namespace listing.
	if err := s.Put(ctx, "other", "k", 1); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, "", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all list = %v %v", all, err)
	}

	if err := s.Delete(ctx, "other", "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "other", "k"); ok {
		t.Fatal("delete did not remove the entry")
	}
}

func TestNilStoreDisabled(t *testing.T) {
	var s *Store
	if s.Backend() != "none" {
		t.Fatal("nil store backend should be none")
	}
	if err := s.Put(context.Background(), "a", "b", "c"); err == nil {
		t.Fatal("nil store Put should fail")
	}
}

func TestStructuredValues(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	type fact struct {
		Topic string `json:"topic"`
		N     int    `json:"n"`
	}
	if err := s.Put(ctx, "facts", "f1", fact{Topic: "go", N: 3}); err != nil {
		t.Fatal(err)
	}
	v, ok, _ := s.Get(ctx, "facts", "f1")
	if !ok || string(v) != `{"topic":"go","n":3}` {
		t.Fatalf("structured value = %s", v)
	}
}
