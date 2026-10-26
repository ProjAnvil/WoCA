package wocadb

import "fmt"

// ConfigAck is one service's self-reported configuration state (design
// §16.7): the settings generation it currently runs. The rows double as a
// liveness heartbeat — a service refreshes its ack on every applied change
// (and the row's age tells the config plane who fell behind or died).
// Capabilities carries comma-separated ability markers (v1: "sandbox")
// the run dispatcher checks before queueing runs (design §10.2 M-4).
type ConfigAck struct {
	ServiceID    string
	Role         string // server | worker | scheduler | config | executor
	Generation   string
	Addr         string
	Capabilities string // comma-separated, e.g. "sandbox"
	// Load is the executor's self-reported in-flight run count (design
	// §3.3 rule 6): the proxy reads it from the heartbeat rows when
	// placing workspaces. Reporters that do not track load leave it zero.
	Load      int64
	UpdatedAt int64
}

// SetConfigAck upserts one service's configuration ack.
func (d *DB) SetConfigAck(ack ConfigAck) error {
	var query string
	switch d.Dialect {
	case DialectPostgres:
		// Placeholders verified by hand: $1 service_id … $7 updated_at,
		// matching the argument order below (the sqlite "?" form cannot
		// catch an off-by-one).
		query = `INSERT INTO config_acks (service_id, role, generation, addr, capabilities, load, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (service_id) DO UPDATE SET
				role = EXCLUDED.role, generation = EXCLUDED.generation,
				addr = EXCLUDED.addr, capabilities = EXCLUDED.capabilities,
				load = EXCLUDED.load, updated_at = EXCLUDED.updated_at`
	default:
		query = `INSERT INTO config_acks (service_id, role, generation, addr, capabilities, load, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (service_id) DO UPDATE SET
				role = excluded.role, generation = excluded.generation,
				addr = excluded.addr, capabilities = excluded.capabilities,
				load = excluded.load, updated_at = excluded.updated_at`
	}
	_, err := d.Exec(query, ack.ServiceID, ack.Role, ack.Generation, ack.Addr, ack.Capabilities, ack.Load, ack.UpdatedAt)
	if err != nil {
		return fmt.Errorf("wocadb: set config_ack %s: %w", ack.ServiceID, err)
	}
	return nil
}

// ConfigAcks returns every recorded service ack (empty when none).
func (d *DB) ConfigAcks() ([]ConfigAck, error) {
	rows, err := d.Query(`SELECT service_id, role, generation, addr, capabilities, load, updated_at FROM config_acks`)
	if err != nil {
		return nil, fmt.Errorf("wocadb: list config_acks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConfigAck
	for rows.Next() {
		var a ConfigAck
		if err := rows.Scan(&a.ServiceID, &a.Role, &a.Generation, &a.Addr, &a.Capabilities, &a.Load, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneConfigAcks deletes acks older than the cutoff (dead service
// generations — hostnames churn on every container recreate).
func (d *DB) PruneConfigAcks(cutoff int64) error {
	if _, err := d.Exec(
		fmt.Sprintf(`DELETE FROM config_acks WHERE updated_at < %s`, d.ph(0)), cutoff,
	); err != nil {
		return fmt.Errorf("wocadb: prune config_acks: %w", err)
	}
	return nil
}
