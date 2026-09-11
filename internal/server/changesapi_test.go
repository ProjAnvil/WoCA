package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
)

// newChangesServer builds a server whose agent owns a real FileHistory
// registry over a temp workspace.
func newChangesServer(t *testing.T) (*Server, string) {
	t.Helper()
	ws := t.TempDir()
	s := New(Config{
		Agent:   newTestAgent(t),
		Static:  fstest.MapFS{},
		Logger:  zerolog.Nop(),
		WorkDir: ws,
		CfgLive: func() config.Config {
			return config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5"}
		},
	})
	return s, ws
}

func postChangesJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestWorkspaceChangesEndpoint verifies GET /api/workspace/changes: pending
// edits render as unified diffs with counts, creations/deletions are
// flagged, and binary files degrade to a flag without diff text.
func TestWorkspaceChangesEndpoint(t *testing.T) {
	s, ws := newChangesServer(t)
	fh := s.agent.FileHistory()
	fh.Record(agent.WithRunWorkspace(context.Background(), ws), ws+"/src/a.go",
		[]byte("package main\n\nfunc f() {}\n"), []byte("package main\n\nfunc f() { return }\n"))
	fh.Record(agent.WithRunWorkspace(context.Background(), ws), ws+"/new.txt",
		nil, []byte("hello\n"))
	fh.Record(agent.WithRunWorkspace(context.Background(), ws), ws+"/img.bin",
		[]byte{0x00, 0x01}, []byte{0x00, 0x02})

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workspace/changes?workspace=" + ws)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Files []changeFile `json:"files"`
		Undo  int          `json:"undo_steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 3 || out.Undo != 3 {
		t.Fatalf("files = %+v, undo = %d", out.Files, out.Undo)
	}
	src := out.Files[0]
	if src.Path != "src/a.go" || !strings.Contains(src.Diff, "-func f() {}") || !strings.Contains(src.Diff, "+func f() { return }") {
		t.Fatalf("src diff wrong: %+v", src)
	}
	if src.Added != 1 || src.Removed != 1 {
		t.Fatalf("counts = %d/%d", src.Added, src.Removed)
	}
	if !out.Files[1].Created || out.Files[1].Added != 1 {
		t.Fatalf("created file wrong: %+v", out.Files[1])
	}
	if !out.Files[2].Binary || out.Files[2].Diff != "" {
		t.Fatalf("binary file wrong: %+v", out.Files[2])
	}
}

// TestWorkspaceUndoEndpoint verifies rollback: path-scoped unwinding and
// the reflected step counts.
func TestWorkspaceUndoEndpoint(t *testing.T) {
	s, ws := newChangesServer(t)
	fh := s.agent.FileHistory()
	ctx := agent.WithRunWorkspace(context.Background(), ws)
	fh.Record(ctx, ws+"/a.txt", []byte("a1"), []byte("a2"))
	fh.Record(ctx, ws+"/b.txt", []byte("b1"), []byte("b2"))
	// Record only logs bytes; materialize the after states so rollback
	// assertions read real files.
	os.WriteFile(ws+"/a.txt", []byte("a2"), 0o644)
	os.WriteFile(ws+"/b.txt", []byte("b2"), 0o644)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// b.txt was touched last: rolling back to before it reverts exactly its
	// step; a.txt (recorded earlier, below it in the stack) keeps its edit.
	resp := postChangesJSON(t, ts.URL+"/api/workspace/undo", map[string]any{"workspace": ws, "path": "b.txt"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Applied int `json:"applied"`
		Undo    int `json:"undo_steps"`
		Redo    int `json:"redo_steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Applied != 1 || out.Undo != 1 || out.Redo != 1 {
		t.Fatalf("undo response = %+v", out)
	}
	if got := readFile(t, ws+"/a.txt"); got != "a2" {
		t.Fatalf("a.txt = %q, want a2 (untouched)", got)
	}
	if got := readFile(t, ws+"/b.txt"); got != "b1" {
		t.Fatalf("b.txt = %q, want b1 (rolled back)", got)
	}
}

// TestTruncateSessionEndpoint verifies the conversation leg of rollback:
// messages after `keep` are dropped and persisted.
func TestTruncateSessionEndpoint(t *testing.T) {
	s, ws := newChangesServer(t)
	sid := "trunc-test"
	if err := s.sessions.save(context.Background(), sid, []messages.Message{
		messages.Human("one"), messages.AI("two"), messages.Human("three"), messages.AI("four"),
	}, ws); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := postChangesJSON(t, ts.URL+"/api/sessions/"+sid+"/truncate", map[string]any{"keep": 2})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Kept int `json:"kept"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Kept != 2 {
		t.Fatalf("kept = %d", out.Kept)
	}
	msgs := s.sessions.load(context.Background(), sid)
	if len(msgs) != 2 || msgs[0].Content != "one" || msgs[1].Content != "two" {
		t.Fatalf("truncated transcript = %v", msgs)
	}
}

// TestBuildPrompt verifies the parked review comments ride the next prompt
// as a file:line-anchored block.
func TestBuildPrompt(t *testing.T) {
	got := buildPrompt("please fix", []runComment{
		{File: "src/auth.go", Line: 47, Text: " don't catch the error here "},
		{File: "web/src/App.vue", Line: 3, Text: "stale import"},
	})
	want := "please fix\n\n<inline-review-comments>\n- src/auth.go:47: don't catch the error here\n- web/src/App.vue:3: stale import\n</inline-review-comments>"
	if got != want {
		t.Fatalf("buildPrompt = %q, want %q", got, want)
	}
	if buildPrompt("plain", nil) != "plain" {
		t.Fatalf("no comments must leave the prompt untouched")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
