package memory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportImportRoundtrip(t *testing.T) {
	src, dst := newMigrationStores(t)
	ctx := t.Context()

	for _, c := range []string{"fact one", "fact two", "fact three"} {
		if _, err := src.Remember(ctx, "/repo", c, []string{"migration"}, "woca:test"); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	n, err := ExportEngine(ctx, src, []string{"/repo"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("exported %d memories", n)
	}

	exported := append([]byte(nil), buf.Bytes()...)
	imported, dropped, err := ImportInto(ctx, dst, bytes.NewReader(exported), "woca:t_import:u_x:/imported")
	if err != nil {
		t.Fatal(err)
	}
	if imported != 3 || dropped != 0 {
		t.Fatalf("import = %d imported, %d dropped", imported, dropped)
	}

	// The wire format is line-delimited JSON with the fields the target
	// needs; nothing else leaks.
	var first map[string]any
	if err := json.Unmarshal(bytes.Split(exported, []byte("\n"))[0], &first); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workspace", "content", "tags", "source", "layer", "type"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("export record missing %q: %v", key, first)
		}
	}
}

func TestImportPreservesContentAndTargetsWorkspace(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			Workspace string   `json:"workspace"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Workspace+"|"+body.Content)
		_, _ = w.Write([]byte(`{"id":"m_1","gated":""}`))
	}))
	defer server.Close()

	dst, err := BuildRemote(RemoteConfig{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	in := strings.NewReader(
		`{"workspace":"/old","content":"alpha","tags":["t"],"source":"woca:test","layer":"L2_semantic","type":"fact","updated_at":1}` + "\n" +
			`{"workspace":"/old","content":"beta","tags":[],"source":"woca:test","layer":"L2_semantic","type":"fact","updated_at":2}` + "\n")
	imported, dropped, err := ImportInto(t.Context(), dst, in, "woca:t:u:/target")
	if err != nil || imported != 2 || dropped != 0 {
		t.Fatalf("import = %d/%d err=%v", imported, dropped, err)
	}
	if len(seen) != 2 || seen[0] != "woca:t:u:/target|alpha" || seen[1] != "woca:t:u:/target|beta" {
		t.Fatalf("targeted writes = %v", seen)
	}
}

func TestImportCountsGateDrops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"","gated":"dropped","reason":"duplicate"}`))
	}))
	defer server.Close()
	dst, err := BuildRemote(RemoteConfig{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	in := strings.NewReader(`{"workspace":"/x","content":"dup","tags":[],"source":"s","layer":"L","type":"fact","updated_at":1}` + "\n")
	imported, dropped, err := ImportInto(t.Context(), dst, in, "/x")
	if err != nil || imported != 0 || dropped != 1 {
		t.Fatalf("drops = %d/%d err=%v", imported, dropped, err)
	}
}

func TestImportRejectsBlankLines(t *testing.T) {
	var seen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		_, _ = w.Write([]byte(`{"id":"m","gated":""}`))
	}))
	defer server.Close()
	dst, _ := BuildRemote(RemoteConfig{Endpoint: server.URL})
	defer dst.Close()

	in := strings.NewReader("\n{\"workspace\":\"/x\",\"content\":\"ok\",\"tags\":[],\"source\":\"s\",\"layer\":\"L\",\"type\":\"fact\",\"updated_at\":1}\n\n")
	if _, _, err := ImportInto(t.Context(), dst, in, "/x"); err != nil {
		t.Fatalf("blank lines rejected: %v", err)
	}
	if seen != 1 {
		t.Fatalf("blank line produced a write: %d", seen)
	}
}

// newMigrationStores builds a real embedded engine (personal sqlite ladyM,
// offline hashing embedding) as the export source.
func newMigrationStores(t *testing.T) (Engine, *RemoteEngine) {
	t.Helper()
	src, err := Build(BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "ladym.db"),
		DefaultWorkspace: "/repo",
		Embedding:        "hashing",
		LLM:              "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	f := newFakeLadyM(t)
	dst, err := BuildRemote(RemoteConfig{Endpoint: f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	return src, dst
}
