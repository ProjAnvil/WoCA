package graph

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
)

// SQLCheckpointer persists checkpoints to the woca database (sqlite or
// postgres — both created by wocadb.Open). Unlike FileCheckpointer it also
// restores the "messages" state channel as []messages.Message so a resumed
// agent run sees properly typed history.
type SQLCheckpointer struct {
	db      *sql.DB
	dialect string
}

// NewSQLCheckpointer wraps an opened *sql.DB. dialect is "sqlite" or
// "postgres" and selects the placeholder style.
func NewSQLCheckpointer(db *sql.DB, dialect string) *SQLCheckpointer {
	return &SQLCheckpointer{db: db, dialect: dialect}
}

// sqlCheckpoint is the row representation of a checkpoint.
type sqlCheckpoint struct {
	Node   string          `json:"node"`
	Values json.RawMessage `json:"values"`
}

// Put implements Checkpointer.
func (s *SQLCheckpointer) Put(key string, cp checkpoint) error {
	if cp.values == nil {
		cp.values = State{}
	}
	values, err := json.Marshal(cp.values)
	if err != nil {
		return err
	}
	switch s.dialect {
	case "postgres":
		_, err = s.db.Exec(`INSERT INTO checkpoints (key, node, "values", updated_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (key) DO UPDATE SET node = EXCLUDED.node, "values" = EXCLUDED."values", updated_at = EXCLUDED.updated_at`,
			key, cp.node, string(values), nowMillis())
	default:
		_, err = s.db.Exec(`INSERT INTO checkpoints (key, node, "values", updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET node = excluded.node, "values" = excluded."values", updated_at = excluded.updated_at`,
			key, cp.node, values, nowMillis())
	}
	return err
}

// Get implements Checkpointer.
func (s *SQLCheckpointer) Get(key string) (checkpoint, bool) {
	var (
		node   string
		values []byte
	)
	q := `SELECT node, "values" FROM checkpoints WHERE key = ?`
	if s.dialect == "postgres" {
		q = `SELECT node, "values" FROM checkpoints WHERE key = $1`
	}
	if err := s.db.QueryRow(q, key).Scan(&node, &values); err != nil {
		return checkpoint{}, false
	}
	state := State{}
	if len(values) > 0 {
		if err := json.Unmarshal(values, &state); err != nil {
			return checkpoint{}, false
		}
		restoreMessages(state)
	}
	return checkpoint{node: node, values: state}, true
}

// restoreMessages re-decodes the "messages" channel as []messages.Message.
// The generic State unmarshal leaves it as []any of raw maps, which would
// break MessagesReducer's type assertions after a cross-process resume.
func restoreMessages(state State) {
	raw, ok := state["messages"]
	if !ok {
		return
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var msgs []messages.Message
	if err := json.Unmarshal(data, &msgs); err == nil && len(msgs) > 0 {
		state["messages"] = msgs
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }
