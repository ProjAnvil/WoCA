package configsvc

// Sandbox backend CRUD tests (design §4.3): registration validation, the
// key-set-only wire surface (the key itself never echoes), the
// single-enabled invariant, and secret cleanup on delete.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/projanvil/woca/internal/settings"
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
