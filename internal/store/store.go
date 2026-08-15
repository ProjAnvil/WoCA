// Package store implements woca's memoryStore: a namespace+key JSON document
// store (the analogue of langgraph's BaseStore) with in-memory, sqlite, and
// postgres backends. The sqlite backend under ~/.woca is the default.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
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

// Put stores value (any JSON-serializable) under namespace/key.
func (s *Store) Put(ctx context.Context, namespace, key string, value any) error {
	if s == nil {
		return fmt.Errorf("store: disabled")
	}
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
	if _, err := s.db.ExecContext(ctx, s.q(
		`INSERT INTO memory (namespace, key, value, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		namespace, key, s.val(data), time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("store: put: %w", err)
	}
	return nil
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
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		e, ok := s.mem[namespace+"/"+key]
		return e.Value, ok, nil
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT value FROM memory WHERE namespace = ? AND key = ?`), namespace, key).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get: %w", err)
	}
	return json.RawMessage(data), true, nil
}

// List returns entries in namespace whose key starts with prefix, ordered by
// key. An empty namespace lists across all namespaces.
func (s *Store) List(ctx context.Context, namespace, prefix string) ([]Entry, error) {
	if s == nil {
		return nil, fmt.Errorf("store: disabled")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []Entry
		for _, e := range s.mem {
			if namespace != "" && e.Namespace != namespace {
				continue
			}
			if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
				continue
			}
			out = append(out, e)
		}
		sortEntries(out)
		return out, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if namespace != "" {
		rows, err = s.db.QueryContext(ctx, s.q(
			`SELECT namespace, key, value, updated_at FROM memory WHERE namespace = ? ORDER BY key`), namespace)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT namespace, key, value, updated_at FROM memory ORDER BY namespace, key`)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ms int64
		if err := rows.Scan(&e.Namespace, &e.Key, &e.Value, &ms); err != nil {
			return nil, err
		}
		e.UpdatedAt = time.UnixMilli(ms)
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes namespace/key (no error when absent).
func (s *Store) Delete(ctx context.Context, namespace, key string) error {
	if s == nil {
		return fmt.Errorf("store: disabled")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.mem, namespace+"/"+key)
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(
		`DELETE FROM memory WHERE namespace = ? AND key = ?`), namespace, key); err != nil {
		return fmt.Errorf("store: delete: %w", err)
	}
	return nil
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Namespace != entries[j].Namespace {
			return entries[i].Namespace < entries[j].Namespace
		}
		return entries[i].Key < entries[j].Key
	})
}
