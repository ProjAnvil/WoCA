package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/memory"
)

func TestMemoryMigrateCmdExportImport(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ladym.db")

	// Seed the personal engine with one fact.
	eng, err := memory.Build(memory.BuildInput{
		EngineEnabled: true, DBPath: dbPath, DefaultWorkspace: "/repo",
		Embedding: "hashing", LLM: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Remember(context.Background(), "/repo", "exported fact", []string{"t"}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "export.jsonl")
	if err := execCmd(t, "memory", "export", "--db", dbPath, "--workspace", "/repo", "--out", out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), "exported fact") {
		t.Fatalf("export = %q err=%v", string(data), err)
	}

	// Import into a fake ladyM enterprise endpoint.
	var gotWorkspace, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content   string `json:"content"`
			Workspace string `json:"workspace"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotWorkspace, gotContent = body.Workspace, body.Content
		_, _ = w.Write([]byte(`{"id":"m_1","gated":""}`))
	}))
	defer srv.Close()

	if err := execCmd(t, "memory", "import", out, "--endpoint", srv.URL, "--workspace", "woca:t:u:/target"); err != nil {
		t.Fatal(err)
	}
	if gotWorkspace != "woca:t:u:/target" || gotContent != "exported fact" {
		t.Fatalf("import got %q / %q", gotWorkspace, gotContent)
	}
	_ = url.Values{}
}

func TestMemoryMigrateCmdValidation(t *testing.T) {
	isolateEnv(t)
	if err := execCmd(t, "memory", "export"); err == nil {
		t.Fatal("export without workspace succeeded")
	}
	if err := execCmd(t, "memory", "import", "/nonexistent", "--endpoint", "http://127.0.0.1:1", "--workspace", "w"); err == nil {
		t.Fatal("import of missing file succeeded")
	}
}
