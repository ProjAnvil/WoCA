package nacosrt

// Pack v2 codec tests: sandbox backends ride the pack, v1 packs decode as
// an empty sandbox set (backward-compatible read), future versions are
// refused loudly.

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func openPackDB(t *testing.T) *wocadb.DB {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "pack.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createSandboxTable(t *testing.T, db *wocadb.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sandbox_backends (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT 'contract-http',
		endpoint TEXT NOT NULL, timeout_ms INTEGER NOT NULL DEFAULT 120000,
		enabled INTEGER NOT NULL DEFAULT 0,
		last_probe_at INTEGER, last_probe_ok INTEGER, last_probe_error TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
}

// createV1Tables provisions the pre-sandbox table set ExportPack reads.
func createV1Tables(t *testing.T, db *wocadb.DB) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS managed_skills (
			name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS managed_mcp (
			name TEXT PRIMARY KEY, spec TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS gw_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackV2RoundTripWithSandboxes(t *testing.T) {
	db := openPackDB(t)
	createV1Tables(t, db)
	createSandboxTable(t, db)
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, last_probe_at, last_probe_ok, last_probe_error, created_at, updated_at)
		VALUES ('sb-1', 'fake', 'contract-http', 'http://127.0.0.1:9999', 120000, 1, 1700000000000, 1, '', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t)

	pack, err := ExportPack(context.Background(), st, db)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if pack.Version != 2 {
		t.Fatalf("pack version = %d want 2", pack.Version)
	}
	if len(pack.Sandboxes) != 1 {
		t.Fatalf("sandboxes = %+v", pack.Sandboxes)
	}
	row := pack.Sandboxes[0]
	if row.ID != "sb-1" || row.Name != "fake" || row.Type != "contract-http" ||
		row.Endpoint != "http://127.0.0.1:9999" || !row.Enabled || !row.LastProbeOK || row.LastProbeAt != 1700000000000 {
		t.Fatalf("row = %+v", row)
	}

	// The key rides Secrets under the sandbox namespace, not the row.
	st.SetSecret("sandbox-api-key:sb-1", "k")
	pack, err = ExportPack(context.Background(), st, db)
	if err != nil {
		t.Fatal(err)
	}
	if pack.Secrets["sandbox-api-key:sb-1"] != "k" {
		t.Fatalf("secret missing from pack: %+v", pack.Secrets)
	}
	// The row itself never carries the key.
	rawRow, _ := json.Marshal(pack.Sandboxes[0])
	if bytes.Contains(rawRow, []byte(`"k"`)) {
		t.Fatalf("row leaks the key: %s", rawRow)
	}

	raw, err := pack.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePack(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Sandboxes) != 1 || decoded.Sandboxes[0].ID != "sb-1" {
		t.Fatalf("decoded sandboxes = %+v", decoded.Sandboxes)
	}
}

// TestPackV1BackwardCompatible: v1 packs predate sandbox backends — they
// decode as an empty set (design §10.2 M-1).
func TestPackV1BackwardCompatible(t *testing.T) {
	p, err := DecodePack(`{"version":1,"generation":"g","settings":{"model":"m"}}`)
	if err != nil {
		t.Fatalf("v1 decode: %v", err)
	}
	if p.Sandboxes != nil {
		t.Fatalf("v1 sandboxes = %+v want nil", p.Sandboxes)
	}
}

// TestPackV3Refused: a future pack must not half-apply.
func TestPackV3Refused(t *testing.T) {
	if _, err := DecodePack(`{"version":3,"generation":"","settings":{}}`); err == nil {
		t.Fatal("v3 pack must be refused")
	}
}

// TestExportWithoutSandboxTable: deployments materializing only the v1
// table set export fine — the sandbox table is skipped silently.
func TestExportWithoutSandboxTable(t *testing.T) {
	db := openPackDB(t)
	// The v1 table set only.
	createV1Tables(t, db)
	st := openTestStore(t)
	pack, err := ExportPack(context.Background(), st, db)
	if err != nil {
		t.Fatalf("export without table: %v", err)
	}
	if pack.Sandboxes != nil {
		t.Fatalf("sandboxes = %+v want nil", pack.Sandboxes)
	}
}
