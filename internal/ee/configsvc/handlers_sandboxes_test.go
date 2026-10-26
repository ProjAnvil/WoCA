package configsvc

// Sandbox backend CRUD tests (design §4.3): registration validation, the
// key-set-only wire surface (the key itself never echoes), the
// single-enabled invariant, and secret cleanup on delete.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func listSandboxes(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	resp := f.do("GET", "/api/sandboxes", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d want 200", resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sandboxIDs(rows []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range rows {
		out[r["id"].(string)] = r
	}
	return out
}

func TestSandboxCRUD(t *testing.T) {
	f := newFixture(t)

	// Create: full validation surface first.
	badType := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "x", "type": "space", "endpoint": "http://sb:1",
	})
	badType.Body.Close()
	if badType.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad type = %d want 400", badType.StatusCode)
	}
	badURL := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "x", "type": "contract-http", "endpoint": "not-a-url",
	})
	badURL.Body.Close()
	if badURL.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad endpoint = %d want 400", badURL.StatusCode)
	}
	noName := f.do("POST", "/api/sandboxes", "", map[string]any{
		"type": "contract-http", "endpoint": "http://sb:1",
	})
	noName.Body.Close()
	if noName.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing name = %d want 400", noName.StatusCode)
	}

	// The type enum is the full contract set: unregistered runtime types
	// (e2b) are still registrable — the registry is a runtime capability.
	created := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "fake", "type": "contract-http", "endpoint": "http://127.0.0.1:1",
		"timeout_ms": 120000, "api_key": "sb-secret-1",
	})
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create = %d want 200", created.StatusCode)
	}

	// Duplicate name: DB constraint surfaces as a friendly 400.
	dup := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "fake", "type": "contract-http", "endpoint": "http://127.0.0.1:2",
	})
	dup.Body.Close()
	if dup.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate name = %d want 400", dup.StatusCode)
	}

	rows := listSandboxes(t, f)
	if len(rows) != 1 {
		t.Fatalf("rows = %d want 1", len(rows))
	}
	id := rows[0]["id"].(string)
	if rows[0]["key_set"] != true {
		t.Fatalf("key_set = %v want true", rows[0]["key_set"])
	}
	if rows[0]["type"] != "contract-http" || rows[0]["endpoint"] != "http://127.0.0.1:1" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0]["timeout_ms"] != float64(120000) {
		t.Fatalf("timeout_ms = %v", rows[0]["timeout_ms"])
	}
	// The key itself never echoes on any CRUD surface.
	body, _ := json.Marshal(rows)
	if bytes.Contains(body, []byte("sb-secret-1")) {
		t.Fatalf("list leaks the key: %s", body)
	}

	// Unregistered type registers fine (runtime capability vs enum).
	e2b := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "e2b-prod", "type": "e2b", "endpoint": "https://e2b.example",
	})
	e2b.Body.Close()
	if e2b.StatusCode != http.StatusOK {
		t.Fatalf("e2b registration = %d want 200", e2b.StatusCode)
	}

	// Partial PUT: only the touched fields change; an empty key keeps the
	// stored secret.
	put := f.do("PUT", "/api/sandboxes/"+id, "", map[string]any{"timeout_ms": 90000})
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put = %d want 200", put.StatusCode)
	}
	if got := f.st.GetSecret("sandbox-api-key:" + id); got != "sb-secret-1" {
		t.Fatalf("empty-key PUT wiped the secret: %q", got)
	}
	after := sandboxIDs(listSandboxes(t, f))[id]
	if after["timeout_ms"] != float64(90000) || after["endpoint"] != "http://127.0.0.1:1" {
		t.Fatalf("partial put = %+v", after)
	}

	// Non-empty key overwrites.
	putKey := f.do("PUT", "/api/sandboxes/"+id, "", map[string]any{"api_key": "sb-secret-2"})
	putKey.Body.Close()
	if putKey.StatusCode != http.StatusOK {
		t.Fatalf("put key = %d want 200", putKey.StatusCode)
	}
	if got := f.st.GetSecret("sandbox-api-key:" + id); got != "sb-secret-2" {
		t.Fatalf("key overwrite = %q", got)
	}

	// Unknown id: 404.
	missing := f.do("PUT", "/api/sandboxes/nope", "", map[string]any{"timeout_ms": 1000})
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id put = %d want 404", missing.StatusCode)
	}
}

func TestSandboxEnabledUniqueAndDelete(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b"} {
		resp := f.do("POST", "/api/sandboxes", "", map[string]any{
			"name": name, "type": "contract-http", "endpoint": "http://127.0.0.1:1", "api_key": "k-" + name,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create %s = %d", name, resp.StatusCode)
		}
	}
	byName := map[string]string{}
	for id, r := range sandboxIDs(listSandboxes(t, f)) {
		byName[r["name"].(string)] = id
	}

	// Enable a: v1 allows exactly one enabled backend.
	on := f.do("PUT", "/api/sandboxes/"+byName["a"], "", map[string]any{"enabled": true})
	on.Body.Close()
	if on.StatusCode != http.StatusOK {
		t.Fatalf("enable a = %d", on.StatusCode)
	}
	// Enable b: a must be disabled in the same write.
	onB := f.do("PUT", "/api/sandboxes/"+byName["b"], "", map[string]any{"enabled": true})
	onB.Body.Close()
	if onB.StatusCode != http.StatusOK {
		t.Fatalf("enable b = %d", onB.StatusCode)
	}
	ids := sandboxIDs(listSandboxes(t, f))
	if ids[byName["a"]]["enabled"] != false || ids[byName["b"]]["enabled"] != true {
		t.Fatalf("single-enabled invariant: a=%v b=%v", ids[byName["a"]]["enabled"], ids[byName["b"]]["enabled"])
	}

	// An enabled backend cannot be deleted (409, disable first).
	del := f.do("DELETE", "/api/sandboxes/"+byName["b"], "", nil)
	del.Body.Close()
	if del.StatusCode != http.StatusConflict {
		t.Fatalf("delete enabled = %d want 409", del.StatusCode)
	}

	// Disable then delete: the row goes and the secret is cleaned.
	off := f.do("PUT", "/api/sandboxes/"+byName["b"], "", map[string]any{"enabled": false})
	off.Body.Close()
	if off.StatusCode != http.StatusOK {
		t.Fatalf("disable b = %d", off.StatusCode)
	}
	if got := f.st.GetSecret("sandbox-api-key:" + byName["b"]); got != "k-b" {
		t.Fatalf("secret before delete = %q", got)
	}
	gone := f.do("DELETE", "/api/sandboxes/"+byName["b"], "", nil)
	gone.Body.Close()
	if gone.StatusCode != http.StatusOK {
		t.Fatalf("delete b = %d", gone.StatusCode)
	}
	if got := f.st.GetSecret("sandbox-api-key:" + byName["b"]); got != "" {
		t.Fatalf("secret not cleaned after delete: %q", got)
	}
	if !f.st.HasSecret(settings.SecretGLMAPIKey) && f.st.HasSecret("sandbox-api-key:"+byName["b"]) {
		t.Fatal("HasSecret still reports the deleted key")
	}
	if len(listSandboxes(t, f)) != 1 {
		t.Fatal("row count after delete != 1")
	}

	// Deleting an unknown id: 404.
	nope := f.do("DELETE", "/api/sandboxes/nope", "", nil)
	nope.Body.Close()
	if nope.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown = %d want 404", nope.StatusCode)
	}
}

// TestSandboxOptionsCRUD (design §17 sandbox params): the six-field
// options object rides create/update, validates via Normalize (invalid →
// 400, nothing stored), and round-trips through the list surface.
func TestSandboxOptionsCRUD(t *testing.T) {
	f := newFixture(t)

	// Invalid network (case-sensitive enum): 400, nothing stored.
	badNet := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "x", "type": "contract-http", "endpoint": "http://sb:1",
		"options": map[string]any{"network": "Bridge"},
	})
	badNet.Body.Close()
	if badNet.StatusCode != http.StatusBadRequest {
		t.Fatalf("bridge network = %d want 400", badNet.StatusCode)
	}
	// Invalid runtime: 400.
	badRT := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "x", "type": "contract-http", "endpoint": "http://sb:1",
		"options": map[string]any{"runtime": "kata"},
	})
	badRT.Body.Close()
	if badRT.StatusCode != http.StatusBadRequest {
		t.Fatalf("kata runtime = %d want 400", badRT.StatusCode)
	}
	// Negative cpu: 400.
	badCPU := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "x", "type": "contract-http", "endpoint": "http://sb:1",
		"options": map[string]any{"cpu": -1},
	})
	badCPU.Body.Close()
	if badCPU.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative cpu = %d want 400", badCPU.StatusCode)
	}
	if rows := listSandboxes(t, f); len(rows) != 0 {
		t.Fatalf("rejected writes stored rows: %+v", rows)
	}

	// Valid create: defaults fill in, env rides, the response echoes.
	created := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "fake", "type": "contract-http", "endpoint": "http://127.0.0.1:1",
		"options": map[string]any{
			"image": "alpine:3", "network": "bridge", "cpu": 2, "memory_mb": 512,
			"env": map[string]any{"A": "1"},
		},
	})
	defer created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create = %d want 200", created.StatusCode)
	}
	var createdRow map[string]any
	if err := json.NewDecoder(created.Body).Decode(&createdRow); err != nil {
		t.Fatal(err)
	}
	opts, _ := createdRow["options"].(map[string]any)
	if opts == nil || opts["network"] != "bridge" || opts["runtime"] != "runc" ||
		opts["cpu"] != float64(2) || opts["memory_mb"] != float64(512) || opts["image"] != "alpine:3" {
		t.Fatalf("created options = %+v", opts)
	}
	if env, _ := opts["env"].(map[string]any); env["A"] != "1" {
		t.Fatalf("created env = %+v", opts["env"])
	}

	id := createdRow["id"].(string)
	if row := sandboxIDs(listSandboxes(t, f))[id]; row["options"].(map[string]any)["network"] != "bridge" {
		t.Fatalf("list options = %+v", row["options"])
	}

	// Invalid PUT: 400, the stored options stay untouched.
	badPut := f.do("PUT", "/api/sandboxes/"+id, "", map[string]any{
		"options": map[string]any{"network": "host"},
	})
	badPut.Body.Close()
	if badPut.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad put = %d want 400", badPut.StatusCode)
	}
	if row := sandboxIDs(listSandboxes(t, f))[id]; row["options"].(map[string]any)["network"] != "bridge" {
		t.Fatalf("bad put mutated options: %+v", row["options"])
	}

	// Valid PUT: the options object is replaced wholesale and re-normalized.
	put := f.do("PUT", "/api/sandboxes/"+id, "", map[string]any{
		"options": map[string]any{"runtime": "runsc", "cpu": 1},
	})
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("put = %d want 200", put.StatusCode)
	}
	got, _ := sandboxIDs(listSandboxes(t, f))[id]["options"].(map[string]any)
	if got["runtime"] != "runsc" || got["network"] != "none" ||
		got["cpu"] != float64(1) || got["image"] != "" {
		t.Fatalf("put options = %+v", got)
	}

	// An options-free PUT keeps the stored options (partial update).
	keep := f.do("PUT", "/api/sandboxes/"+id, "", map[string]any{"timeout_ms": 90000})
	keep.Body.Close()
	if keep.StatusCode != http.StatusOK {
		t.Fatalf("keep put = %d want 200", keep.StatusCode)
	}
	if got, _ := sandboxIDs(listSandboxes(t, f))[id]["options"].(map[string]any); got["runtime"] != "runsc" {
		t.Fatalf("options wiped by partial put: %+v", got)
	}
}

// TestSandboxOptionsRideThePack: options written through the console land
// in the exported config pack — the delivery path the executor reads.
func TestSandboxOptionsRideThePack(t *testing.T) {
	f := newFixture(t)
	created := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "fake", "type": "contract-http", "endpoint": "http://127.0.0.1:1",
		"options": map[string]any{
			"network": "bridge", "cpu": 2, "memory_mb": 512, "image": "alpine:3",
			"env": map[string]any{"A": "1"},
		},
	})
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create = %d want 200", created.StatusCode)
	}
	pack, err := nacosrt.ExportPack(context.Background(), f.st, f.db)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Sandboxes) != 1 || pack.Sandboxes[0].Options == nil {
		t.Fatalf("pack sandboxes = %+v", pack.Sandboxes)
	}
	o := pack.Sandboxes[0].Options
	if o.Network != "bridge" || o.Runtime != "runc" || o.CPU != 2 ||
		o.MemoryMB != 512 || o.Image != "alpine:3" || o.Env["A"] != "1" {
		t.Fatalf("pack options = %+v", o)
	}
}

// TestSandboxOptionsLegacyColumnUpgrade: a database created by an earlier
// build (sandbox_backends without the options column) is upgraded in
// place by New — existing rows survive with empty options.
func TestSandboxOptionsLegacyColumnUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := "sqlite:" + filepath.Join(dir, "cfg.db")
	db, err := wocadb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
		VALUES ('sb-old', 'legacy', 'contract-http', 'http://old:1', 120000, 0, 1, 2)`); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, ts, db, zerolog.Nop())
	if err != nil {
		t.Fatalf("New over legacy schema: %v", err)
	}
	svc.AttachPublisher(&fakePublisher{})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)

	// The legacy row survived and reads without options.
	resp, err := http.Get(srv.URL + "/api/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list over upgraded schema = %d", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != "sb-old" || rows[0]["options"] != nil {
		t.Fatalf("legacy rows = %+v", rows)
	}

	// And a write with options persists on the upgraded table.
	body := bytes.NewBufferString(`{"options":{"network":"bridge","cpu":2}}`)
	put, err := http.NewRequest(http.MethodPut, srv.URL+"/api/sandboxes/sb-old", body)
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("Content-Type", "application/json")
	presp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("put over upgraded schema = %d", presp.StatusCode)
	}
	list, err := http.Get(srv.URL + "/api/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var after []map[string]any
	if err := json.NewDecoder(list.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if opts, _ := after[0]["options"].(map[string]any); opts["network"] != "bridge" {
		t.Fatalf("options after upgrade write = %+v", after[0]["options"])
	}
}
