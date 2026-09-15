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

// TestRunScopesToolsToSessionWorkspace is the regression test for the
// cross-workspace bug: a run started in session workspace B must resolve
// relative tool paths in B, not in the server's startup directory.
func TestRunScopesToolsToSessionWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "marker.txt"), []byte("from-base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("from-session-ws"), 0o644); err != nil {
		t.Fatal(err)
	}

	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Read", Args: map[string]any{"file_path": "marker.txt"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(base),
		SystemPrompt:   "startup",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
		WorkDir:        base,
		SystemPromptFor: func(dir, model string) string {
			return "ENV:" + dir
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: base})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := fmt.Sprintf(`{"prompt":"read the marker","workspace":%q}`, ws)
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	toolResult := readRunToolResult(t, ts.URL, created.RunID)
	if !strings.Contains(toolResult, "from-session-ws") {
		t.Fatalf("Read marker.txt = %q, want from-session-ws (resolved in the session workspace)", toolResult)
	}
}

// readRunToolResult subscribes to a run's SSE stream and returns the first
// tool result content (failing the test on errors).
func readRunToolResult(t *testing.T, base, runID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
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
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before a tool result")
		}
		switch ev.Type {
		case "tool_result":
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
				t.Fatalf("bad tool_result payload %q: %v", ev.Data, err)
			}
			return payload.Content
		case "error":
			t.Fatalf("run error: %s", ev.Data)
		}
	}
}

func TestWorkspaceEndpointsScopeToQueryWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "base.md"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "session.md"), []byte("from-ws"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: base})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Default: the server's workdir (backwards compatible).
	var root struct {
		Children []struct {
			Name string `json:"name"`
		} `json:"children"`
	}
	resp, err := http.Get(ts.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(root.Children) != 1 || root.Children[0].Name != "base.md" {
		t.Fatalf("default workspace children = %+v, want [base.md]", root.Children)
	}

	// Scoped: the requested session workspace.
	resp, err = http.Get(ts.URL + "/api/workspace?workspace=" + ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(root.Children) != 1 || root.Children[0].Name != "session.md" {
		t.Fatalf("scoped workspace children = %+v, want [session.md]", root.Children)
	}

	// The file endpoint follows the same parameter.
	resp, err = http.Get(ts.URL + "/api/workspace/file?workspace=" + ws + "&path=session.md")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if file.Content != "from-ws" {
		t.Fatalf("scoped file content = %q, want from-ws", file.Content)
	}
}
