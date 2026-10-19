package configsvc

// Verify probe tests (design §4.3): the full-chain liveness probe runs
// against a real contract v2 backend (fakesandbox), persists the
// last_probe_* columns, and never echoes the stored key.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
)

func verifyResp(t *testing.T, f *fixture, id string) (int, map[string]any, []byte) {
	t.Helper()
	resp := f.do("POST", "/api/sandboxes/"+id+"/verify", "", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("verify body: %v (%s)", err, raw)
		}
	}
	return resp.StatusCode, doc, raw
}

// createBackend registers one contract-http backend and returns its id.
func createBackend(t *testing.T, f *fixture, name, endpoint, key string) string {
	t.Helper()
	resp := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": name, "type": "contract-http", "endpoint": endpoint, "api_key": key,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create backend = %d", resp.StatusCode)
	}
	for _, r := range listSandboxes(t, f) {
		if r["name"] == name {
			return r["id"].(string)
		}
	}
	t.Fatal("backend not listed after create")
	return ""
}

func TestSandboxVerifyGreen(t *testing.T) {
	f := newFixture(t)
	fake := fakesandbox.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	id := createBackend(t, f, "fake", srv.URL, "sb-verify-secret")
	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d", status)
	}
	if doc["ok"] != true {
		t.Fatalf("ok = %v (%s)", doc["ok"], raw)
	}
	if bytes.Contains(raw, []byte("sb-verify-secret")) {
		t.Fatalf("verify leaks the key: %s", raw)
	}
	steps, _ := doc["steps"].([]any)
	names := []string{}
	for _, st := range steps {
		m, _ := st.(map[string]any)
		if m["ok"] != true {
			t.Fatalf("step %v failed: %v", m["name"], m["error"])
		}
		names = append(names, m["name"].(string))
	}
	for _, want := range []string{"create", "exec", "file", "glob", "stat", "process", "archive"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("step %q missing from %v", want, names)
		}
	}
	if _, ok := doc["total_latency_ms"]; !ok {
		t.Fatal("total_latency_ms missing")
	}

	// The probe result persists on the row.
	row, ok, err := f.svc.getSandbox(id)
	if err != nil || !ok {
		t.Fatalf("row read: %v ok=%v", err, ok)
	}
	if !row.LastProbeOK || row.LastProbeAt == 0 || row.LastProbeError != "" {
		t.Fatalf("last_probe = %+v", row)
	}
}

func TestSandboxVerifyBadEndpoint(t *testing.T) {
	f := newFixture(t)
	// Port 1 on the loopback: connection refused — the first step (the
	// provider's create/provision call) must fail and the failure must
	// persist.
	id := createBackend(t, f, "dead", "http://127.0.0.1:1", "")

	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d", status)
	}
	if doc["ok"] != false {
		t.Fatalf("ok = %v want false (%s)", doc["ok"], raw)
	}
	steps, _ := doc["steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("no steps reported")
	}
	first, _ := steps[0].(map[string]any)
	if first["name"] != "create" || first["ok"] != false {
		t.Fatalf("first step = %v want failed create", first)
	}

	row, ok, err := f.svc.getSandbox(id)
	if err != nil || !ok {
		t.Fatalf("row read: %v", err)
	}
	if row.LastProbeOK || row.LastProbeAt == 0 || row.LastProbeError == "" {
		t.Fatalf("persisted failure = %+v", row)
	}
}

func TestSandboxVerifyUnregisteredType(t *testing.T) {
	f := newFixture(t)
	// e2b is in the type enum (registrable) but has no runtime provider:
	// verify must report the gap, not crash.
	resp := f.do("POST", "/api/sandboxes", "", map[string]any{
		"name": "e2b", "type": "e2b", "endpoint": "https://e2b.example",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register e2b = %d", resp.StatusCode)
	}
	var id string
	for _, r := range listSandboxes(t, f) {
		if r["name"] == "e2b" {
			id = r["id"].(string)
		}
	}
	status, doc, raw := verifyResp(t, f, id)
	if status != http.StatusOK {
		t.Fatalf("verify = %d (%s)", status, raw)
	}
	if doc["ok"] != false {
		t.Fatalf("ok = %v want false", doc["ok"])
	}
	steps, _ := doc["steps"].([]any)
	first, _ := steps[0].(map[string]any)
	if first["name"] != "create" || first["ok"] != false {
		t.Fatalf("first step = %v", first)
	}
	if !strings.Contains(first["error"].(string), "not available") {
		t.Fatalf("error = %v want provider-not-available", first["error"])
	}
}
