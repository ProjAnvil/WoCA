package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadManagedInventoryEmpty(t *testing.T) {
	t.Setenv("WOCA_HOME", t.TempDir())
	inv := ReadManagedInventory()
	if inv.Skills == nil || inv.MCP == nil {
		t.Fatalf("empty inventory must be empty lists, not nils: %+v", inv)
	}
	if len(inv.Skills) != 0 || len(inv.MCP) != 0 {
		t.Fatalf("expected empty inventory, got %+v", inv)
	}
}

// The inventory must mirror exactly what applyManaged materialized — the
// same round trip the settings page relies on.
func TestReadManagedInventoryAfterApply(t *testing.T) {
	t.Setenv("WOCA_HOME", t.TempDir())
	if err := materializeManagedSkills(`[
		{"name":"deploy","description":"ship the stack","body":"steps here"},
		{"name":"audit","body":"checks"}
	]`); err != nil {
		t.Fatal(err)
	}
	if err := materializeManagedMCP(`[
		"search=https://search.internal/mcp",
		"fs=npx -y fs-mcp"
	]`); err != nil {
		t.Fatal(err)
	}

	inv := ReadManagedInventory()
	if len(inv.Skills) != 2 {
		t.Fatalf("skills = %+v", inv.Skills)
	}
	if inv.Skills[0].Name != "audit" || inv.Skills[0].Description != "" {
		t.Fatalf("skills[0] = %+v", inv.Skills[0])
	}
	if inv.Skills[1].Name != "deploy" || inv.Skills[1].Description != "ship the stack" {
		t.Fatalf("skills[1] = %+v", inv.Skills[1])
	}
	if len(inv.MCP) != 2 {
		t.Fatalf("mcp = %+v", inv.MCP)
	}
	if inv.MCP[0].Name != "fs" || inv.MCP[0].Type != "stdio" || inv.MCP[0].Command != "sh -c npx -y fs-mcp" {
		t.Fatalf("mcp[0] = %+v", inv.MCP[0])
	}
	if inv.MCP[1].Name != "search" || inv.MCP[1].Type != "http" || inv.MCP[1].URL != "https://search.internal/mcp" {
		t.Fatalf("mcp[1] = %+v", inv.MCP[1])
	}
}

func TestReadManagedMCPUnknownShapeSkipped(t *testing.T) {
	root := t.TempDir()
	doc := `{"mcpServers":{
		"good":{"type":"http","url":"https://x.internal/mcp"},
		"weird":{"totally":"unknown"}
	}}`
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readManagedMCP(root)
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("readManagedMCP = %+v", got)
	}
}

func TestManagedInventoryRoutes(t *testing.T) {
	t.Setenv("WOCA_HOME", t.TempDir())
	if err := materializeManagedSkills(`[{"name":"deploy","description":"ship","body":"b"}]`); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	calls := 0
	ManagedInventoryRoutes(mux, func(h http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			h(w, r)
		})
	})

	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/managed", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if calls != 1 {
		t.Fatalf("wrap calls = %d", calls)
	}
	if ct := resp.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var inv ManagedInventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Skills) != 1 || inv.Skills[0].Name != "deploy" {
		t.Fatalf("inventory = %+v", inv)
	}

	// The un-wrapped form registers too (local-trust deployments).
	bare := http.NewServeMux()
	ManagedInventoryRoutes(bare, nil)
	resp2 := httptest.NewRecorder()
	bare.ServeHTTP(resp2, httptest.NewRequest(http.MethodGet, "/api/managed", nil))
	if resp2.Code != http.StatusOK {
		t.Fatalf("bare status = %d", resp2.Code)
	}
}
