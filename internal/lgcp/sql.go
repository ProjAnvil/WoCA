package lgcp

import (
	"context"
	"database/sql"
	"fmt"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
)

// SQLSaver persists checkpoints in a SQL database (sqlite or postgres via
// wocadb). It implements the fork's checkpoint.Saver over the shared saver
// core; the whole record (checkpoint + metadata + pending writes + slots)
// is stored as one JSON blob per checkpoint, keyed by thread/namespace/ID.
type SQLSaver struct {
	core    *saver
	db      *sql.DB
	dialect string
	ser     lgcheckpoint.Serializer
}

// Saver delegation.

// GetTuple implements lgcheckpoint.Saver.
func (s *SQLSaver) GetTuple(ctx context.Context, cfg lgcheckpoint.Config) (*lgcheckpoint.Tuple, error) {
	return s.core.GetTuple(ctx, cfg)
}

// List implements lgcheckpoint.Saver.
func (s *SQLSaver) List(ctx context.Context, cfg lgcheckpoint.Config, opts lgcheckpoint.ListOptions) ([]lgcheckpoint.Tuple, error) {
	return s.core.List(ctx, cfg, opts)
}

// Put implements lgcheckpoint.Saver.
func (s *SQLSaver) Put(ctx context.Context, cfg lgcheckpoint.Config, cp lgcheckpoint.Checkpoint, md lgcheckpoint.Metadata, newVersions map[string]int64) (lgcheckpoint.Config, error) {
	return s.core.Put(ctx, cfg, cp, md, newVersions)
}

// PutWrites implements lgcheckpoint.Saver.
func (s *SQLSaver) PutWrites(ctx context.Context, cfg lgcheckpoint.Config, writes []lgcheckpoint.Write, taskID, taskPath string) error {
	return s.core.PutWrites(ctx, cfg, writes, taskID, taskPath)
}

// DeleteThread implements lgcheckpoint.Saver.
func (s *SQLSaver) DeleteThread(ctx context.Context, threadID string) error {
	return s.core.DeleteThread(ctx, threadID)
}

// NewSQLSaver creates a SQL-backed Saver. dialect is "sqlite" or "postgres"
// (see wocadb.Dialect); the schema is created on demand.
func NewSQLSaver(db *sql.DB, dialect string) *SQLSaver {
	ser := newSerializer()
	s := &SQLSaver{db: db, dialect: dialect, ser: ser}
	s.core = newSaver(s, ser)
	return s
}

// compile-time interface checks.
var (
	_ lgcheckpoint.Saver = (*FileSaver)(nil)
	_ lgcheckpoint.Saver = (*SQLSaver)(nil)
	_ store              = (*fileStore)(nil)
	_ store              = (*SQLSaver)(nil)
)

const sqlSchema = `CREATE TABLE IF NOT EXISTS lgcp_checkpoints (
	thread_id TEXT NOT NULL,
	ns TEXT NOT NULL,
	checkpoint_id TEXT NOT NULL,
	record TEXT NOT NULL,
	PRIMARY KEY (thread_id, ns, checkpoint_id)
)`

func (s *SQLSaver) ensureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqlSchema); err != nil {
		return fmt.Errorf("lgcp: create schema: %w", err)
	}
	return nil
}

func (s *SQLSaver) get(ctx context.Context, thread, ns, id string) (*record, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	if id == "" {
		row := s.db.QueryRowContext(ctx,
			`SELECT checkpoint_id, record FROM lgcp_checkpoints WHERE thread_id = ? AND ns = ? ORDER BY checkpoint_id DESC LIMIT 1`, thread, ns)
		var gotID, blob string
		if err := row.Scan(&gotID, &blob); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil
			}
			return nil, err
		}
		return s.decode(blob)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT record FROM lgcp_checkpoints WHERE thread_id = ? AND ns = ? AND checkpoint_id = ?`, thread, ns, id)
	var blob string
	if err := row.Scan(&blob); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return s.decode(blob)
}

func (s *SQLSaver) listIDs(ctx context.Context, thread, ns string) ([]string, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT checkpoint_id FROM lgcp_checkpoints WHERE thread_id = ? AND ns = ?`, thread, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLSaver) put(ctx context.Context, thread, ns, id string, rec record) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	blob, err := encodeRecord(s.ser, rec)
	if err != nil {
		return err
	}
	var query string
	switch s.dialect {
	case "postgres":
		query = `INSERT INTO lgcp_checkpoints (thread_id, ns, checkpoint_id, record) VALUES ($1, $2, $3, $4)
			ON CONFLICT (thread_id, ns, checkpoint_id) DO UPDATE SET record = EXCLUDED.record`
	default: // sqlite (wocadb's other dialect)
		query = `INSERT INTO lgcp_checkpoints (thread_id, ns, checkpoint_id, record) VALUES (?, ?, ?, ?)
			ON CONFLICT (thread_id, ns, checkpoint_id) DO UPDATE SET record = excluded.record`
	}
	_, err = s.db.ExecContext(ctx, query, thread, ns, id, string(blob))
	return err
}

func (s *SQLSaver) delThread(ctx context.Context, thread string) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM lgcp_checkpoints WHERE thread_id = ?`, thread)
	return err
}

func (s *SQLSaver) decode(blob string) (*record, error) {
	rec, err := decodeRecord(s.ser, []byte(blob))
	if err != nil {
		return nil, err
	}
	return &rec, nil
}
