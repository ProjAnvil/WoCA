package memcmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/memory"
	"github.com/spf13/cobra"
)

func exec(t *testing.T, args ...string) error {
	t.Helper()
	cmd := New()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

func TestExportImportRoundtrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ladym.db")

	eng, err := memory.Build(memory.BuildInput{
		EngineEnabled: true, DBPath: dbPath, DefaultWorkspace: "/w",
		Embedding: "hashing", LLM: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Remember(t.Context(), "/w", "cli roundtrip fact", nil, "test"); err != nil {
		t.Fatal(err)
	}
	_ = eng.Close()

	out := filepath.Join(dir, "export.jsonl")
	if err := exec(t, "export", "--db", dbPath, "--workspace", "/w", "--out", out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		t.Fatalf("export empty: %v", err)
	}

	var gotWS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Workspace string `json:"workspace"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotWS = body.Workspace
		_, _ = w.Write([]byte(`{"id":"m","gated":""}`))
	}))
	defer srv.Close()

	if err := exec(t, "import", out, "--endpoint", srv.URL, "--workspace", "woca:t:u:/target"); err != nil {
		t.Fatal(err)
	}
	if gotWS != "woca:t:u:/target" {
		t.Fatalf("target = %q", gotWS)
	}
}

func TestExportRequiresWorkspace(t *testing.T) {
	if err := exec(t, "export"); err == nil {
		t.Fatal("export without workspace succeeded")
	}
}

func TestImportMissingFile(t *testing.T) {
	if err := exec(t, "import", "/nonexistent", "--endpoint", "http://x", "--workspace", "w"); err == nil {
		t.Fatal("import missing file succeeded")
	}
}

var _ = cobra.Command{}
