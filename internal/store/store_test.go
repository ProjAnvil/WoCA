package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
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

func TestStoreAccessorsAndBackendNames(t *testing.T) {
	var nilStore *Store
	if got := nilStore.Backend(); got != "none" {
		t.Errorf("nil Backend = %q", got)
	}
	if got := nilStore.Path(); got != "disabled" {
		t.Errorf("nil Path = %q", got)
	}

	mem := NewMemory()
	if got := mem.Backend(); got != "memory" {
		t.Errorf("memory Backend = %q", got)
	}
	if got := mem.Path(); got != "memory" {
		t.Errorf("memory Path = %q", got)
	}

	db, err := wocadb.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sql := NewSQL(db.DB, string(db.Dialect), db.Path)
	if got := sql.Backend(); got != string(db.Dialect) {
		t.Errorf("sql Backend = %q", got)
	}
	if got := sql.Path(); got != db.Path {
		t.Errorf("sql Path = %q", got)
	}

	// Closed-database degradation: writes fail, reads fall back.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sql.Put(context.Background(), "ns", "k", map[string]any{"a": 1}); err == nil {
		t.Error("closed Put should fail")
	}
	if _, ok, err := sql.Get(context.Background(), "ns", "k"); ok || err == nil {
		t.Error("closed Get should miss with an error")
	}
	if got, _ := sql.List(context.Background(), "ns", ""); len(got) != 0 {
		t.Errorf("closed List = %v", got)
	}
	if err := sql.Delete(context.Background(), "ns", "k"); err == nil {
		t.Error("closed Delete should fail")
	}
}

func TestSortEntriesOrdering(t *testing.T) {
	entries := []Entry{
		{Namespace: "b", Key: "x"},
		{Namespace: "a", Key: "z"},
		{Namespace: "a", Key: "m"},
	}
	sortEntries(entries)
	if entries[0].Namespace != "a" || entries[0].Key != "m" || entries[2].Namespace != "b" {
		t.Fatalf("sorted = %+v", entries)
	}
}

func TestPostgresPlaceholderRewrite(t *testing.T) {
	// The postgres dialect rewrites ? placeholders; sqlite passes through.
	sqlite := &Store{dialect: "sqlite"}
	if got := sqlite.q("SELECT * FROM t WHERE a = ? AND b = ?"); got != "SELECT * FROM t WHERE a = ? AND b = ?" {
		t.Fatalf("sqlite q = %q", got)
	}
	db, err := wocadb.Open(filepath.Join(t.TempDir(), "pg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pg := &Store{db: db.DB, dialect: "postgres"}
	if got := pg.q("SELECT * FROM t WHERE a = ? AND b = ?"); got != "SELECT * FROM t WHERE a = $1 AND b = $2" {
		t.Fatalf("pg q = %q", got)
	}
	// val() serializes to string for postgres, bytes otherwise.
	if got := pg.val([]byte("{}")); got != "{}" {
		t.Fatalf("pg val = %v", got)
	}
	if got := (&Store{}).val([]byte("{}")); string(got.([]byte)) != "{}" {
		t.Fatalf("sqlite val = %v", got)
	}
}

func TestNilStoreAllOpsRefuse(t *testing.T) {
	var s *Store
	ctx := context.Background()
	if _, _, err := s.Get(ctx, "ns", "k"); err == nil {
		t.Error("nil store Get should fail")
	}
	if _, err := s.List(ctx, "ns", ""); err == nil {
		t.Error("nil store List should fail")
	}
	if err := s.Delete(ctx, "ns", "k"); err == nil {
		t.Error("nil store Delete should fail")
	}
}

func TestPutMarshalFailure(t *testing.T) {
	s := NewMemory()
	if err := s.Put(context.Background(), "ns", "k", make(chan int)); err == nil {
		t.Fatal("unmarshalable value should fail Put")
	}
}

func TestMemoryListFilters(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	for _, e := range []struct{ ns, key string }{
		{"alpha", "k1"}, {"alpha", "k2"}, {"beta", "k1"},
	} {
		if err := s.Put(ctx, e.ns, e.key, 1); err != nil {
			t.Fatal(err)
		}
	}
	// Namespace filter: beta's entry is skipped while listing alpha.
	entries, err := s.List(ctx, "alpha", "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("alpha list = %v, %v", entries, err)
	}
	// Key-prefix filter skips non-matching keys.
	entries, err = s.List(ctx, "alpha", "k2")
	if err != nil || len(entries) != 1 || entries[0].Key != "k2" {
		t.Fatalf("prefix list = %v, %v", entries, err)
	}
	// Cross-namespace listing with a prefix only common to some keys.
	entries, err = s.List(ctx, "", "k2")
	if err != nil || len(entries) != 1 {
		t.Fatalf("cross-ns prefix list = %v, %v", entries, err)
	}
}

// TestSQLListFilteringAndScanFailure drives the SQL list path's row-level
// filters plus its Scan error: a NULL namespace column cannot be scanned
// into the string field.
func TestSQLListFilteringAndScanFailure(t *testing.T) {
	db, err := wocadb.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewSQL(db.DB, string(db.Dialect), db.Path)
	ctx := context.Background()

	if err := s.Put(ctx, "facts", "keep-1", 1); err != nil {
		t.Fatal(err)
	}
	// A tenant-prefixed row written out-of-band: invisible to the local tenant.
	if _, err := db.DB.Exec(`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('tenant:x/facts', 'keep-2', '1', 0)`); err != nil {
		t.Fatal(err)
	}
	// A row whose key does not match the prefix exercises the row-level
	// prefix skip; the tenant row exercises the visibility skip.
	if _, err := db.DB.Exec(`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('facts', 'unrelated', '1', 0)`); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx, "", "keep")
	if err != nil || len(entries) != 1 || entries[0].Namespace != "facts" {
		t.Fatalf("filtered list = %v, %v — tenant row should be invisible", entries, err)
	}

	if _, err := db.DB.Exec(`INSERT INTO memory (namespace, key, value, updated_at) VALUES ('facts', 'bad', '1', 'not-a-timestamp')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(ctx, "", ""); err == nil {
		t.Fatal("unscannable updated_at row should surface a Scan error")
	}
}

// TestPostgresTenantBeginErrors drives the tenant transaction prologue on
// the postgres path: BeginTx failure (closed pool) and set_config failure
// (sqlite has no such function), for every entry point that calls begin.
func TestPostgresTenantBeginErrors(t *testing.T) {
	newPg := func(t *testing.T) (*Store, *wocadb.DB) {
		t.Helper()
		db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "pg.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return NewSQL(db.DB, "postgres", db.Path), db
	}
	tenant := tenancy.NewContext(context.Background(), tenancy.Context{TenantID: "t_pg", UserID: "u"})

	t.Run("closed pool", func(t *testing.T) {
		s, db := newPg(t)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(tenant, "ns", "k", 1); err == nil {
			t.Error("Put with failed BeginTx should error")
		}
		if _, _, err := s.Get(tenant, "ns", "k"); err == nil {
			t.Error("Get with failed BeginTx should error")
		}
		if _, err := s.List(tenant, "ns", ""); err == nil {
			t.Error("List with failed BeginTx should error")
		}
		if err := s.Delete(tenant, "ns", "k"); err == nil {
			t.Error("Delete with failed BeginTx should error")
		}
	})

	t.Run("set_config unsupported", func(t *testing.T) {
		s, _ := newPg(t)
		// BeginTx succeeds on sqlite, but set_config is postgres-only, so the
		// scoped transaction fails and must roll back.
		if err := s.Put(tenant, "ns", "k", 1); err == nil {
			t.Error("Put without tenant GUC should error")
		}
		if _, _, err := s.Get(tenant, "ns", "k"); err == nil {
			t.Error("Get without tenant GUC should error")
		}
		if _, err := s.List(tenant, "ns", ""); err == nil {
			t.Error("List without tenant GUC should error")
		}
		if err := s.Delete(tenant, "ns", "k"); err == nil {
			t.Error("Delete without tenant GUC should error")
		}
	})
}

// Closed-DB error sweep: every store method surfaces the DB error.
func TestStoreClosedDBSweep(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "closed.db")
	db, err := wocadb.Open("sqlite:" + dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSQL(db.DB, string(db.Dialect), "")
	ctx := context.Background()
	_ = s.Put(ctx, "ns", "k", "v")
	_ = db.Close()

	// Every method must return an error, not panic.
	if _, _, err := s.Get(ctx, "ns", "k"); err == nil {
		t.Fatal("Get on closed DB succeeded")
	}
	if err := s.Put(ctx, "ns", "k", "v"); err == nil {
		t.Fatal("Put on closed DB succeeded")
	}
	if err := s.Delete(ctx, "ns", "k"); err == nil {
		t.Fatal("Delete on closed DB succeeded")
	}
	if _, err := s.List(ctx, "ns", ""); err == nil {
		t.Fatal("List on closed DB succeeded")
	}

}
