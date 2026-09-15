// Package store implements woca's memoryStore: a namespace+key JSON document
// store (the analogue of langgraph's BaseStore) with in-memory, sqlite, and
// postgres backends. The sqlite backend under ~/.woca is the default.
//
// Tenancy (design doc §9.1, Phase 1): namespaces of non-local tenants are
// prefixed "tenant:<id>/" — the application-level isolation predicate. On
// postgres each such call also runs inside a transaction with
// SET LOCAL app.tenant_id so row-level security tags writes and filters
// reads with the same tenant (belt and suspenders with the prefix). The
// local machine tenant keeps its historical unprefixed namespaces, so
// existing data and the personal edition are unaffected.
package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// Entry is one stored memory value.
type Entry struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store is a memoryStore. Exactly one backend is active: an in-process map
// or a SQL database (sqlite or postgres).
type Store struct {
	mem     map[string]Entry
	memKeys map[string]time.Time
	db      *sql.DB
	dialect string
	path    string

	mu sync.Mutex
}

// NewMemory creates an in-process (non-persistent) store.
func NewMemory() *Store {
	return &Store{
		mem:     map[string]Entry{},
		memKeys: map[string]time.Time{},
		path:    "memory",
	}
}

// NewSQL creates a store backed by an opened woca database.
func NewSQL(db *sql.DB, dialect, path string) *Store {
	return &Store{db: db, dialect: dialect, path: path}
}

// Backend reports the active backend name (memory | sqlite | postgres).
func (s *Store) Backend() string {
	if s == nil {
		return "none"
	}
	if s.db != nil {
		return s.dialect
	}
	return "memory"
}

// Path describes the storage location for display.
func (s *Store) Path() string {
	if s == nil {
		return "disabled"
	}
	return s.path
}

func (s *Store) q(query string) string {
	if s.db == nil || s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

// querier is the database surface common to *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// scopePrefix returns the namespace prefix isolating the ctx's tenant, or
// "" for the local machine tenant (single-tenant mode keeps historical
// unprefixed namespaces).
func scopePrefix(ctx context.Context) string {
	tc, ok := tenancy.FromContext(ctx)
	if !ok || tc.TenantID == "" || tc.TenantID == "local" {
		return ""
	}
	return "tenant:" + tc.TenantID + "/"
}

// begin returns the querier for one store call. Local-tenant calls use the
// shared pool directly. Non-local tenants on postgres run in a transaction
// carrying SET LOCAL app.tenant_id (the GUC the RLS policy compares
// against), so writes are tagged and reads filtered at the row level; the
// returned finish function commits on ok and rolls back otherwise. On
// sqlite a non-local tenant is isolated by the namespace prefix alone
// (sqlite is the single-tenant backend; the route layer denies remote
// tenants host surfaces in Phase 1).
func (s *Store) begin(ctx context.Context) (q querier, finish func(ok bool) error, err error) {
	tc, _ := tenancy.FromContext(ctx)
	if tc.TenantID == "" || tc.TenantID == "local" || s.db == nil || s.dialect != "postgres" {
		return s.db, func(bool) error { return nil }, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("store: tenant tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tc.TenantID); err != nil {
		_ = tx.Rollback()
		return nil, nil, fmt.Errorf("store: tenant tx scope: %w", err)
	}
	return tx, func(ok bool) error {
		if ok {
			return tx.Commit()
		}
		return tx.Rollback()
	}, nil
}

// Put stores value (any JSON-serializable) under namespace/key.
func (s *Store) Put(ctx context.Context, namespace, key string, value any) error {
	if s == nil {
		return fmt.Errorf("store: disabled")
	}
	namespace = scopePrefix(ctx) + namespace
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("store: marshal value: %w", err)
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.mem[namespace+"/"+key] = Entry{Namespace: namespace, Key: key, Value: data, UpdatedAt: time.Now()}
		return nil
	}
	q, finish, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, s.q(
		`INSERT INTO memory (namespace, key, value, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		namespace, key, s.val(data), time.Now().UnixMilli()); err != nil {
		_ = finish(false)
		return fmt.Errorf("store: put: %w", err)
	}
	return finish(true)
}

// val adapts the JSON bytes for the driver: pgx sends []byte as bytea, so
// TEXT columns need a string.
func (s *Store) val(data []byte) any {
	if s.dialect == "postgres" {
		return string(data)
	}
	return data
}

// Get returns the raw JSON value stored under namespace/key.
func (s *Store) Get(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("store: disabled")
	}
	namespace = scopePrefix(ctx) + namespace
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.mem[namespace+"/"+key]
		return e.Value, ok, nil
	}
	q, finish, err := s.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = finish(false) }()
	var data []byte
	err = q.QueryRowContext(ctx, s.q(
		`SELECT value FROM memory WHERE namespace = ? AND key = ?`), namespace, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get: %w", err)
	}
	return json.RawMessage(data), true, nil
}

// List returns entries in namespace whose key starts with prefix, ordered by
// key. An empty namespace lists across all namespaces — scoped to the
// caller's tenant: a non-local tenant never sees other tenants' entries nor
// the local machine's unprefixed ones, and the local machine tenant never
// sees tenant-scoped entries (matching the postgres RLS policy).
func (s *Store) List(ctx context.Context, namespace, prefix string) ([]Entry, error) {
	if s == nil {
		return nil, fmt.Errorf("store: disabled")
	}
	scope := scopePrefix(ctx)
	// The tenant prefix must not turn an across-namespaces list into an
	// exact-match one: emptiness is decided on the caller's argument.
	acrossNamespaces := namespace == ""
	namespace = scope + namespace
	visible := func(ns string) bool {
		if scope != "" {
			return strings.HasPrefix(ns, scope)
		}
		return !strings.HasPrefix(ns, "tenant:")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []Entry
		for _, e := range s.mem {
			if !acrossNamespaces && e.Namespace != namespace {
				continue
			}
			if !visible(e.Namespace) {
				continue
			}
			if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
				continue
			}
			e.Namespace = strings.TrimPrefix(e.Namespace, scope)
			out = append(out, e)
		}
		sortEntries(out)
		return out, nil
	}
	q, finish, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = finish(false) }()
	var (
		rows *sql.Rows
	)
	if !acrossNamespaces {
		rows, err = q.QueryContext(ctx, s.q(
			`SELECT namespace, key, value, updated_at FROM memory WHERE namespace = ? ORDER BY key`), namespace)
	} else {
		rows, err = q.QueryContext(ctx,
			`SELECT namespace, key, value, updated_at FROM memory ORDER BY namespace, key`)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ms int64
		// Scan the JSON value as []byte: the postgres driver delivers TEXT
		// as string, which database/sql will not assign to *json.RawMessage.
		var raw []byte
		if err := rows.Scan(&e.Namespace, &e.Key, &raw, &ms); err != nil {
			return nil, err
		}
		e.Value = raw
		e.UpdatedAt = time.UnixMilli(ms)
		if !visible(e.Namespace) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		e.Namespace = strings.TrimPrefix(e.Namespace, scope)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes namespace/key (no error when absent).
func (s *Store) Delete(ctx context.Context, namespace, key string) error {
	if s == nil {
		return fmt.Errorf("store: disabled")
	}
	namespace = scopePrefix(ctx) + namespace
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.mem, namespace+"/"+key)
		return nil
	}
	q, finish, err := s.begin(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, s.q(
		`DELETE FROM memory WHERE namespace = ? AND key = ?`), namespace, key); err != nil {
		_ = finish(false)
		return fmt.Errorf("store: delete: %w", err)
	}
	return finish(true)
}

func sortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Key, b.Key),
		)
	})
}
