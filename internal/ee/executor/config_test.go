package executor

// Config-plane tests (design §10.2 M-4, §17): the params source reads the
// materialized sandbox_backends row — options included — at construction,
// serves it from memory, and Refresh (the pack OnChange hook's call) is the
// only thing that moves it; the executor ack body reports Role=executor
// with the "sandbox" capability exactly while the config is enabled and the
// caller-injected live load.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/worker"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func openConfigDB(t *testing.T) *wocadb.DB {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "exec-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// createBackendsTable mirrors the nacosrt worker-side DDL (unexported
// there): the executor only ever sees the materialized table, so the test
// provisions it directly instead of driving a full pack apply.
func createBackendsTable(t *testing.T, db *wocadb.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sandbox_backends (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT 'contract-http',
		endpoint TEXT NOT NULL,
		timeout_ms INTEGER NOT NULL DEFAULT 120000,
		enabled INTEGER NOT NULL DEFAULT 0,
		last_probe_at INTEGER,
		last_probe_ok INTEGER,
		last_probe_error TEXT,
		options TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
}

func insertBackend(t *testing.T, db *wocadb.DB, id string, enabled bool, opts *nacosrt.BackendOptions) {
	t.Helper()
	optsText, err := nacosrt.EncodeOptionsText(opts)
	if err != nil {
		t.Fatal(err)
	}
	var e int64
	if enabled {
		e = 1
	}
	now := wocadb.Now()
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, options, created_at, updated_at)
		VALUES (?, ?, 'contract-http', 'http://sb:1', 120000, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, options = excluded.options, updated_at = excluded.updated_at`,
		id, "backend-"+id, e, optsText, now, now); err != nil {
		t.Fatal(err)
	}
}

// TestParamsSourceSnapshotNormalizedOptions: a materialized enabled row
// with options yields Enabled + Normalize-filled defaults (network/runtime
// filled, values kept verbatim).
func TestParamsSourceSnapshotNormalizedOptions(t *testing.T) {
	db := openConfigDB(t)
	createBackendsTable(t, db)
	insertBackend(t, db, "sb-1", true, &nacosrt.BackendOptions{
		Image: "alpine:3", CPU: 2, MemoryMB: 512, Env: map[string]string{"A": "1"},
	})

	snap := NewParamsSource(db).Snapshot()
	if !snap.Enabled {
		t.Fatal("snapshot disabled with an enabled materialized row")
	}
	if o := snap.Options; o.Image != "alpine:3" || o.Runtime != "runc" || o.Network != "none" ||
		o.CPU != 2 || o.MemoryMB != 512 || o.Env["A"] != "1" {
		t.Fatalf("snapshot options = %+v, want normalized defaults", snap.Options)
	}
}

// TestParamsSourceDisabledWithoutEnabledRow: no row, or only a disabled
// row, means Enabled=false and a zero (not Normalize-defaulted) options
// set — the executor must not mistake defaults for a configured backend.
func TestParamsSourceDisabledWithoutEnabledRow(t *testing.T) {
	db := openConfigDB(t)
	createBackendsTable(t, db)

	if snap := NewParamsSource(db).Snapshot(); snap.Enabled || snap.Options.Runtime != "" || snap.Options.Network != "" {
		t.Fatalf("empty table snapshot = %+v, want disabled zero params", snap)
	}

	insertBackend(t, db, "sb-1", false, &nacosrt.BackendOptions{Image: "alpine:3"})
	if snap := NewParamsSource(db).Snapshot(); snap.Enabled {
		t.Fatalf("disabled-row snapshot = %+v, want disabled", snap)
	}
}

// TestParamsSourceRefreshPicksUpPackChange: Snapshot serves the cached
// value — only Refresh (the OnChange hook) moves it, in both directions
// (enable and full-manifest disable).
func TestParamsSourceRefreshPicksUpPackChange(t *testing.T) {
	db := openConfigDB(t)
	createBackendsTable(t, db)
	src := NewParamsSource(db)
	if src.Snapshot().Enabled {
		t.Fatal("enabled with an empty table")
	}

	insertBackend(t, db, "sb-1", true, &nacosrt.BackendOptions{Image: "a:1"})
	if src.Snapshot().Enabled {
		t.Fatal("Snapshot served fresh DB state without Refresh — cache broken")
	}
	src.Refresh()
	if snap := src.Snapshot(); !snap.Enabled || snap.Options.Image != "a:1" {
		t.Fatalf("post-refresh snapshot = %+v, want enabled a:1", snap)
	}

	insertBackend(t, db, "sb-1", false, nil)
	src.Refresh()
	if src.Snapshot().Enabled {
		t.Fatal("snapshot still enabled after the pack removed the enabled row")
	}
}

// TestAckFuncBuildsExecutorAck: the ack body carries Role=executor, the
// generation, the "sandbox" capability exactly while enabled, the
// caller's live load, and the routable advertise address (the proxy
// dials ack.Addr — an empty address is unroutable); nil hooks degrade to
// zero values.
func TestAckFuncBuildsExecutorAck(t *testing.T) {
	db := openConfigDB(t)
	createBackendsTable(t, db)
	insertBackend(t, db, "sb-1", true, nil)
	src := NewParamsSource(db)

	ack := AckFunc("exec-1", "http://exec-1:8093", func() string { return "g7" }, src, func() int64 { return 3 })
	if a := ack(); a.ServiceID != "exec-1" || a.Role != "executor" || a.Generation != "g7" ||
		a.Capabilities != "sandbox" || a.Load != 3 || a.Addr != "http://exec-1:8093" {
		t.Fatalf("ack = %+v", a)
	}

	insertBackend(t, db, "sb-1", false, nil)
	src.Refresh()
	if a := ack(); a.Capabilities != "" {
		t.Fatalf("disabled ack capabilities = %q, want empty", a.Capabilities)
	}

	if a := AckFunc("exec-2", "", nil, nil, nil)(); a.Generation != "" || a.Capabilities != "" || a.Load != 0 {
		t.Fatalf("nil-hook ack = %+v, want zeros", a)
	}
}

// TestAckFuncNormalizesAdvertise: the proxy routes on ack.Addr as a URL
// (scheme + host required), so a bare host:port — which the flag default
// produces — is normalized to an http:// URL inside the tested boundary,
// not left to the caller's discipline.
func TestAckFuncNormalizesAdvertise(t *testing.T) {
	tests := []struct {
		advertise string
		want      string
	}{
		{"exec-1:8093", "http://exec-1:8093"}, // bare host:port (flag default shape)
		{"http://e:1", "http://e:1"},          // already absolute
		{"https://e:1", "https://e:1"},        // other schemes pass through
		{"localhost", "http://localhost"},     // bare host, no port
		{"", ""},                              // empty stays empty (unroutable by design)
	}
	for _, tt := range tests {
		if a := AckFunc("exec-1", tt.advertise, nil, nil, nil)(); a.Addr != tt.want {
			t.Fatalf("AckFunc advertise %q -> addr %q, want %q", tt.advertise, a.Addr, tt.want)
		}
	}
}

// TestExecutorAckHeartbeatRow: the parameterized worker heartbeat fed the
// executor ack body writes the row the dispatcher/proxy read — Role=
// executor, sandbox capability, load from the injected counter (design
// §3.3 rule 6, §10.2 M-4).
func TestExecutorAckHeartbeatRow(t *testing.T) {
	db := openConfigDB(t)
	createBackendsTable(t, db)
	insertBackend(t, db, "sb-1", true, nil)
	src := NewParamsSource(db)

	w := worker.New(worker.Config{ID: "exec-1", Logger: zerolog.Nop(), DB: db})
	go w.AckHeartbeat(t.Context(), 10*time.Millisecond,
		AckFunc("exec-1", "http://exec-1:8093", func() string { return "g7" }, src, func() int64 { return 5 }))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		acks, err := db.ConfigAcks()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range acks {
			if a.ServiceID == "exec-1" && a.Role == "executor" && a.Generation == "g7" &&
				a.Capabilities == "sandbox" && a.Load == 5 && a.UpdatedAt != 0 &&
				a.Addr == "http://exec-1:8093" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("executor heartbeat never rewrote the ack row")
}
