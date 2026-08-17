package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newUndoServer builds a server with a real agent whose registry tracks file
// changes in dir, plus a fake model that writes a file via the Write tool.
func newUndoServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{
					"file_path": filepath.Join(dir, "out.txt"),
					"content":   "from agent",
				}},
			}},
			messages.Message{Role: messages.RoleAI, Content: "wrote the file"},
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		WorkDir:        dir,
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop()}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// driveCommandRun streams a run's events to completion and returns the
// concatenated delta text.
func driveCommandRun(t *testing.T, url, runID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var buf strings.Builder
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		switch ev.Type {
		case "delta":
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(ev.Data), &payload) == nil {
				buf.WriteString(payload.Text)
			}
		case "error":
			t.Fatalf("run error: %s", ev.Data)
		case "done":
			return buf.String()
		}
	}
}

func TestUndoRedoCommandRuns(t *testing.T) {
	dir := t.TempDir()
	ts := newUndoServer(t, dir)

	// Turn 1: the agent writes out.txt (records file history).
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(`{"prompt":"write it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	driveCommandRun(t, ts.URL, created.RunID)
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("agent did not write the file: %v", err)
	}

	post := func(prompt string) string {
		resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(fmt.Sprintf(`{"prompt":%q,"session_id":%q}`, prompt, created.SessionID)))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			RunID string `json:"run_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return driveCommandRun(t, ts.URL, c.RunID)
	}

	// /undo reverts the agent's write and does NOT hit the model.
	if out := post("/undo"); !strings.Contains(out, "Undid") {
		t.Fatalf("/undo output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("out.txt still present after /undo: %v", err)
	}
	// /redo restores it.
	if out := post("/redo"); !strings.Contains(out, "Redid") {
		t.Fatalf("/redo output = %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
	if string(data) != "from agent" {
		t.Fatalf("out.txt = %q after redo", data)
	}

	// The command turns persisted into the session transcript.
	sess, err := http.Get(ts.URL + "/api/sessions/" + created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Body.Close()
	var transcript struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(sess.Body).Decode(&transcript); err != nil {
		t.Fatal(err)
	}
	var undoTurns int64
	for _, m := range transcript.Messages {
		if m.Role == "human" && strings.TrimSpace(m.Content) == "/undo" {
			atomic.AddInt64(&undoTurns, 1)
		}
	}
	if undoTurns != 1 {
		t.Fatalf("session should carry the /undo turn, found %d", undoTurns)
	}
}
