package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

// ctxAs returns a context authenticated as the given tenant.
func ctxAs(t *testing.T, tenantID string) context.Context {
	t.Helper()
	return tenancy.NewContext(t.Context(), tenancy.Context{TenantID: tenantID, UserID: "u1"})
}

// TestTenantIsolation exercises the Phase 1 acceptance criterion "two tenants
// are invisible to each other" at the store layer: entries written under one
// tenant are invisible to the other tenant and to the local machine tenant,
// on every backend.
func TestTenantIsolation(t *testing.T) {
	newStore := func(t *testing.T) *Store {
		t.Helper()
		return NewMemory()
	}
	testIsolation(t, "memory", newStore)

	testIsolation(t, "sqlite", func(t *testing.T) *Store {
		db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return NewSQL(db.DB, string(db.Dialect), db.Path)
	})
}

func testIsolation(t *testing.T, backend string, newStore func(*testing.T) *Store) {
	t.Helper()
	t.Run(backend, func(t *testing.T) {
		s := newStore(t)
		local := t.Context()
		a := ctxAs(t, "t_aaa")
		b := ctxAs(t, "t_bbb")

		// Each identity writes the same namespace/key with its own value.
		for _, c := range []struct {
			ctx context.Context
			val string
		}{{local, "local"}, {a, "A"}, {b, "B"}} {
			if err := s.Put(c.ctx, "facts", "k", c.val); err != nil {
				t.Fatal(err)
			}
		}

		get := func(ctx context.Context) string {
			v, ok, err := s.Get(ctx, "facts", "k")
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				return "<none>"
			}
			return string(v)
		}
		if got := get(local); got != `"local"` {
			t.Errorf("local sees %s, want its own value", got)
		}
		if got := get(a); got != `"A"` {
			t.Errorf("tenant A sees %s, want its own value", got)
		}
		if got := get(b); got != `"B"` {
			t.Errorf("tenant B sees %s, want its own value", got)
		}

		// Cross-namespace listing stays inside the tenant.
		list := func(ctx context.Context) int {
			entries, err := s.List(ctx, "", "")
			if err != nil {
				t.Fatal(err)
			}
			return len(entries)
		}
		if n := list(local); n != 1 {
			t.Errorf("local lists %d entries, want 1 (its own)", n)
		}
		if n := list(a); n != 1 {
			t.Errorf("tenant A lists %d entries, want 1", n)
		}
		if n := list(b); n != 1 {
			t.Errorf("tenant B lists %d entries, want 1", n)
		}

		// Cross-tenant delete is a no-op: B cannot delete A's entry.
		if err := s.Delete(b, "facts", "k"); err != nil {
			t.Fatal(err)
		}
		if got := get(a); got != `"A"` {
			t.Errorf("after tenant B delete, A sees %s — cross-tenant delete leaked", got)
		}

		// Namespaced listing under the tenant prefix works and hides others.
		if err := s.Put(a, "other", "x", `1`); err != nil {
			t.Fatal(err)
		}
		entries, err := s.List(a, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Errorf("tenant A lists %d entries, want 2", len(entries))
		}
		for _, e := range entries {
			if e.Namespace != "facts" && e.Namespace != "other" {
				t.Errorf("tenant A sees foreign namespace %q", e.Namespace)
			}
		}
	})
}

// TestLocalUnchanged: with no tenant context, namespaces and keys keep their
// exact historical form (backward compatibility for existing data).
func TestLocalUnchanged(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()
	if err := s.Put(ctx, "facts", "k", "v"); err != nil {
		t.Fatal(err)
	}
	e, ok, _ := s.Get(ctx, "facts", "k")
	if !ok || string(e) != `"v"` {
		t.Fatalf("get = %s %v", e, ok)
	}
	s.mu.Lock()
	_, has := s.mem["facts/k"]
	s.mu.Unlock()
	if !has {
		t.Fatal("local namespace was prefixed — breaks existing data")
	}
}
