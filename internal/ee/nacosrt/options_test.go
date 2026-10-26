package nacosrt

// BackendOptions tests (design §17 sandbox params): Normalize's defaults
// and refusals, options riding the pack codec, the materialized-table
// round trip Task 8's executor reads, and the legacy-column upgrade.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBackendOptionsNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      *BackendOptions
		want    *BackendOptions
		wantErr bool
	}{
		{"empty fills defaults", &BackendOptions{},
			&BackendOptions{Network: "none", Runtime: "runc"}, false},
		{"explicit values preserved", &BackendOptions{
			Image: "alpine:3", Runtime: "runsc", Network: "bridge",
			CPU: 2, MemoryMB: 512, Env: map[string]string{"A": "1"},
		}, &BackendOptions{
			Image: "alpine:3", Runtime: "runsc", Network: "bridge",
			CPU: 2, MemoryMB: 512, Env: map[string]string{"A": "1"},
		}, false},
		{"network Bridge rejected (case-sensitive)", &BackendOptions{Network: "Bridge"}, nil, true},
		{"network host rejected", &BackendOptions{Network: "host"}, nil, true},
		{"runtime kata rejected", &BackendOptions{Runtime: "kata"}, nil, true},
		{"negative cpu rejected", &BackendOptions{CPU: -1}, nil, true},
		{"negative memory rejected", &BackendOptions{MemoryMB: -1}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Normalize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Fatalf("Normalize() = %+v, want %+v", tt.in, tt.want)
			}
		})
	}
	// A nil options set is "no options": a no-op, not a panic.
	var nilOpts *BackendOptions
	if err := nilOpts.Normalize(); err != nil {
		t.Fatalf("nil Normalize() = %v, want nil", err)
	}
}

// TestPackCarriesOptions: options stored as JSON text ride the pack as a
// decoded struct; a row without options carries no options key at all
// (the pack bytes of a pre-options row are unchanged).
func TestPackCarriesOptions(t *testing.T) {
	db := openPackDB(t)
	createV1Tables(t, db)
	createSandboxTable(t, db)
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, options, created_at, updated_at)
		VALUES ('sb-1', 'with-opts', 'contract-http', 'http://127.0.0.1:9999', 120000, 1,
		'{"image":"alpine:3","runtime":"runsc","network":"bridge","cpu":2,"memory_mb":512,"env":{"A":"1"}}', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, created_at, updated_at)
		VALUES ('sb-2', 'plain', 'contract-http', 'http://127.0.0.2:9999', 60000, 0, 1, 2)`); err != nil {
		t.Fatal(err)
	}
	pack, err := ExportPack(context.Background(), openTestStore(t), db)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Sandboxes) != 2 {
		t.Fatalf("sandboxes = %+v", pack.Sandboxes)
	}
	// ORDER BY name: "plain" sorts before "with-opts".
	if pack.Sandboxes[0].ID != "sb-2" || pack.Sandboxes[0].Options != nil {
		t.Fatalf("plain row = %+v", pack.Sandboxes[0])
	}
	o := pack.Sandboxes[1].Options
	if o == nil || o.Image != "alpine:3" || o.Runtime != "runsc" || o.Network != "bridge" ||
		o.CPU != 2 || o.MemoryMB != 512 || o.Env["A"] != "1" {
		t.Fatalf("options = %+v", o)
	}
	// The options key appears only on the row that carries them.
	rowJSON, err := json.Marshal(pack.Sandboxes[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rowJSON, []byte("options")) {
		t.Fatalf("plain row carries options: %s", rowJSON)
	}
	raw, err := pack.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"options":{"image":"alpine:3"`) {
		t.Fatalf("options missing from encoded pack: %s", raw)
	}
}

// TestMaterializeSandboxOptions: the materialize INSERT carries options
// into the worker-side table, and EnabledSandbox reads them back — the
// executor's read path.
func TestMaterializeSandboxOptions(t *testing.T) {
	rt := newTestRuntime(t)
	pack := &Pack{Version: packVersion, Generation: "g1", Sandboxes: []SandboxRow{
		{ID: "sb-1", Name: "a", Type: "contract-http", Endpoint: "http://a:1", TimeoutMs: 120000, Enabled: true,
			Options: &BackendOptions{Image: "alpine:3", Runtime: "runsc", Network: "bridge",
				CPU: 2, MemoryMB: 512, Env: map[string]string{"A": "1"}}},
	}}
	if err := rt.apply(context.Background(), pack); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var opts string
	if err := rt.cfg.DB.QueryRow(`SELECT options FROM sandbox_backends WHERE id = 'sb-1'`).Scan(&opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts, `"network":"bridge"`) || !strings.Contains(opts, `"runtime":"runsc"`) {
		t.Fatalf("stored options = %s", opts)
	}
	row, err := EnabledSandbox(rt.cfg.DB.DB)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Options == nil || row.Options.Network != "bridge" ||
		row.Options.Runtime != "runsc" || row.Options.Env["A"] != "1" {
		t.Fatalf("enabled = %+v", row)
	}
}

// TestEnsureSandboxBackendsUpgradesLegacyTable: a table from an earlier
// build (no options column) gains the column in place, keeping its rows;
// re-running is idempotent.
func TestEnsureSandboxBackendsUpgradesLegacyTable(t *testing.T) {
	db := openPackDB(t)
	if _, err := db.Exec(`CREATE TABLE sandbox_backends (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT 'contract-http', endpoint TEXT NOT NULL,
		timeout_ms INTEGER NOT NULL DEFAULT 120000, enabled INTEGER NOT NULL DEFAULT 0,
		last_probe_at INTEGER, last_probe_ok INTEGER, last_probe_error TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, created_at, updated_at)
		VALUES ('sb-old', 'legacy', 'contract-http', 'http://old:1', 120000, 1, 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureSandboxBackends(db); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	var opts string
	if err := db.QueryRow(`SELECT options FROM sandbox_backends WHERE id = 'sb-old'`).Scan(&opts); err != nil {
		t.Fatalf("options column missing after upgrade: %v", err)
	}
	if opts != "" {
		t.Fatalf("legacy row options = %q, want \"\"", opts)
	}
	if err := ensureSandboxBackends(db); err != nil {
		t.Fatalf("second ensure (idempotence): %v", err)
	}
}
