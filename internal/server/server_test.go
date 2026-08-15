package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/slashcmds"
	"github.com/projanvil/woca/internal/tools"
)

func newTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	// First response asks a question (interrupts), second answers in prose.
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "AskUserQuestion", Args: map[string]any{
					"questions": []any{
						map[string]any{"question": "pick one"},
					},
				}},
			}},
			messages.AI("thanks"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunStreamsAndResolvesInterrupt(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, _ := createRun(t, ts.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	// Phase 1: read until the interrupt arrives.
	toolCallSeen := false
	interruptKind := ""
	for interruptKind == "" {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before interrupt")
		}
		switch ev.Type {
		case "tool_call":
			toolCallSeen = true
		case "interrupt":
			var payload struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
				t.Fatalf("bad interrupt payload %q: %v", ev.Data, err)
			}
			interruptKind = payload.Kind
		}
	}
	if !toolCallSeen {
		t.Fatal("expected a tool_call event before the interrupt")
	}
	if interruptKind != "ask" {
		t.Fatalf("interrupt kind = %q, want ask", interruptKind)
	}

	// Phase 2: answer the question and read until the run finishes.
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", `{"answers":["A"]}`); err != nil {
		t.Fatal(err)
	}
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		switch ev.Type {
		case "done":
			return
		case "error":
			t.Fatalf("run failed: %s", ev.Data)
		}
	}
}

func TestStaticServingAndSPAFallback(t *testing.T) {
	static := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><html>app</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('hi')")},
	}
	s := New(Config{Static: static, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cases := []struct {
		path string
		want string
	}{
		{"/", "app"},
		{"/assets/app.js", "console.log"},
		{"/sessions/123", "app"}, // SPA fallback for client-side routes
	}
	for _, c := range cases {
		resp, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", c.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), c.want) {
			t.Fatalf("GET %s: body %q does not contain %q", c.path, body, c.want)
		}
	}
}

func TestSessionStoreRoundTrip(t *testing.T) {
	st := newSessionStore(t.TempDir())
	msgs := []messages.Message{
		messages.Human("hi"),
		{Role: messages.RoleTool, ToolCallID: "1", Content: "tool result"},
		messages.AI("done"),
	}
	if err := st.save("s1", msgs, ""); err != nil {
		t.Fatal(err)
	}
	got := st.load("s1")
	if len(got) != len(msgs) {
		t.Fatalf("len = %d, want %d", len(got), len(msgs))
	}
	if got[1].Role != messages.RoleTool || got[1].ToolCallID != "1" || got[1].Content != "tool result" {
		t.Fatalf("tool message not preserved: %+v", got[1])
	}
}

func TestSessionPersistenceAndList(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, sessionID := createRun(t, ts.URL, "")
	driveToDone(t, ts.URL, runID)

	// The transcript (including the tool result) is persisted under the session.
	resp, err := http.Get(ts.URL + "/api/sessions/" + sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		ID       string `json:"id"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != sessionID {
		t.Fatalf("session id = %q, want %q", got.ID, sessionID)
	}
	foundTool := false
	for _, m := range got.Messages {
		if m.Role == "tool" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("transcript should contain a tool result, got roles: %+v", got.Messages)
	}

	// The session appears in the list endpoint.
	listResp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		if s.ID == sessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("session %q not found in list", sessionID)
	}
}

func TestWorkspace(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("README.md", "# hello")
	mustWrite("main.go", "package main")
	mustWrite("docs/design.md", "## design")
	mustWrite("node_modules/dep/index.js", "// should be excluded")

	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var root struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Children []struct {
			Name     string `json:"name"`
			Kind     string `json:"kind"`
			Content  string `json:"content"`
			Children []struct {
				Name    string `json:"name"`
				Kind    string `json:"kind"`
				Content string `json:"content"`
			} `json:"children"`
		} `json:"children"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root.Kind != "dir" {
		t.Fatalf("root kind = %q, want dir", root.Kind)
	}

	fileContent := map[string]string{}
	for _, c := range root.Children {
		if c.Kind == "file" {
			fileContent[c.Name] = c.Content
		}
		if c.Name == "node_modules" {
			t.Fatal("node_modules should be excluded")
		}
	}
	if fileContent["README.md"] != "# hello" {
		t.Fatalf("README.md content = %q, want %q", fileContent["README.md"], "# hello")
	}
	if got, ok := fileContent["main.go"]; !ok || got != "" {
		t.Fatalf("main.go should be listed without content, got %q (ok=%v)", got, ok)
	}

	foundDesign := false
	for _, c := range root.Children {
		if c.Name != "docs" {
			continue
		}
		for _, d := range c.Children {
			if d.Name == "design.md" && d.Content == "## design" {
				foundDesign = true
			}
		}
	}
	if !foundDesign {
		t.Fatal("docs/design.md not found with content")
	}
}

func TestWorkspaceFileEndpoint(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("img.png", "\x00\x01\x02binary")
	write("README.md", "# hello")

	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(ts.URL + "/api/workspace/file?path=" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := get("main.go"); code != http.StatusOK || !strings.Contains(body, "package main") {
		t.Fatalf("main.go: status %d body %s", code, body)
	}
	if code, body := get("README.md"); code != http.StatusOK || !strings.Contains(body, "# hello") {
		t.Fatalf("README.md: status %d body %s", code, body)
	}
	if code, _ := get("img.png"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("img.png: status %d, want 415", code)
	}
	if code, _ := get("missing.go"); code != http.StatusNotFound {
		t.Fatalf("missing.go: status %d, want 404", code)
	}
	if code, _ := get("../escape.go"); code != http.StatusNotFound {
		t.Fatalf("traversal: status %d, want 404", code)
	}
}

func TestSessionListGroupsByWorkspace(t *testing.T) {
	dir := t.TempDir()
	st := newSessionStore(dir)
	st.defaultWorkspace = "/tmp/current-project"

	// Legacy session (no metadata recorded) falls back to the default workspace.
	if err := st.save("legacy", []messages.Message{messages.Human("old")}, ""); err != nil {
		t.Fatal(err)
	}
	// New session explicitly tagged with another workspace.
	if err := st.save("tagged", []messages.Message{messages.Human("new")}, "/tmp/other-project"); err != nil {
		t.Fatal(err)
	}

	list, err := st.list()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]sessionSummary{}
	for _, s := range list {
		byID[s.ID] = s
	}
	if byID["legacy"].Workspace != "/tmp/current-project" {
		t.Fatalf("legacy workspace = %q, want default", byID["legacy"].Workspace)
	}
	if byID["legacy"].WorkspaceName != "current-project" {
		t.Fatalf("legacy workspace name = %q", byID["legacy"].WorkspaceName)
	}
	if byID["tagged"].Workspace != "/tmp/other-project" || byID["tagged"].WorkspaceName != "other-project" {
		t.Fatalf("tagged workspace = %+v", byID["tagged"])
	}
}

func TestCompletionsEndpoint(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	s.agent.SetSkills([]skills.Skill{{Name: "commit", Description: "create a commit"}})
	s.agent.SetAgentDefinitions([]agents.Definition{{Name: "reviewer", Description: "code review"}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Skills []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"skills"`
		Agents []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	byName := map[string]string{}
	for _, e := range got.Skills {
		byName[e.Name] = e.Description
	}
	// Builtin slash commands (the TUI console's table) are all present…
	for _, c := range slashcmds.Builtin() {
		if byName[c.Name] != c.Description {
			t.Fatalf("builtin command %q missing from completions (got %q)", c.Name, byName[c.Name])
		}
	}
	// …and the agent's skills are merged in.
	if byName["commit"] != "create a commit" {
		t.Fatalf("skill commit missing: %+v", byName)
	}
	if len(got.Skills) != len(slashcmds.Builtin())+1 {
		t.Fatalf("skills count = %d, want %d", len(got.Skills), len(slashcmds.Builtin())+1)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "reviewer" || got.Agents[0].Description != "code review" {
		t.Fatalf("agents = %+v", got.Agents)
	}
}

func TestRunWithImageAttachment(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{
		Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: dir,
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1x1 red PNG.
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	body := fmt.Sprintf(`{"prompt":"what is this?","attachments":[{"name":"shot.png","mime_type":"image/png","data":%q}]}`, png)
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.RunID == "" {
		t.Fatal("no run id")
	}
	driveToDone(t, ts.URL, created.RunID)

	// The persisted transcript carries the image block on the human message.
	sess, err := http.Get(ts.URL + "/api/sessions/" + created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Body.Close()
	var transcript struct {
		Messages []struct {
			Role          string `json:"role"`
			ContentBlocks []struct {
				Type     string `json:"type"`
				MimeType string `json:"mime_type"`
				Base64   string `json:"base64"`
			} `json:"content_blocks"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(sess.Body).Decode(&transcript); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range transcript.Messages {
		if m.Role != "human" {
			continue
		}
		for _, b := range m.ContentBlocks {
			if b.Type == "image" && b.MimeType == "image/png" && b.Base64 == png {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("human message should carry the image block, got %+v", transcript.Messages)
	}

	// Invalid attachments are rejected before the run starts.
	bad := `{"prompt":"x","attachments":[{"name":"a.exe","mime_type":"application/octet-stream","data":"YQ=="}]}`
	resp, err = http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad attachment status = %d, want 400", resp.StatusCode)
	}
}

// TestRunScopesMemoryToSessionWorkspace drives a full run whose session
// belongs to a non-default workspace and asserts the memory subsystem agrees
// with the panel: auto-retain writes AND turn prefetch both resolve the
// request workspace through the run ctx (not the agent-global field).
func TestRunScopesMemoryToSessionWorkspace(t *testing.T) {
	f := &fakeMemEngine{}
	a := newTestAgent(t)
	a.SetMemoryEngine(f)
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: "/ws/server"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"prompt":"how does auth work?","workspace":"/ws/session"}`
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.RunID == "" {
		t.Fatal("no run id")
	}
	driveToDone(t, ts.URL, created.RunID)

	// RetainRun completes before the "done" event, so the fact is already in.
	if len(f.remembered) != 1 || !strings.HasPrefix(f.remembered[0], "/ws/session|woca:auto|") {
		t.Fatalf("remembered = %v, want the retained fact in /ws/session", f.remembered)
	}
	// Prefetch is asynchronous: poll for its Recall to land on the same
	// workspace the memory panel would query.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ws, _ := f.recordedRecall(); ws == "/ws/session" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, ws, _ := f.recordedRecall()
	t.Fatalf("prefetch recall workspace = %q, want /ws/session", ws)
}

func TestHealth(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("health = %v, want ok=true", body)
	}
}

// --- helpers ---

func createRun(t *testing.T, base, sessionID string) (string, string) {
	t.Helper()
	body := fmt.Sprintf(`{"prompt":"hi","session_id":%q}`, sessionID)
	resp, err := http.Post(base+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.RunID == "" {
		t.Fatal("empty run_id")
	}
	if created.SessionID == "" {
		t.Fatal("empty session_id")
	}
	return created.RunID, created.SessionID
}

// driveToDone subscribes to a run's SSE stream, answers the first interrupt,
// and blocks until the run reaches a terminal state.
func driveToDone(t *testing.T, base, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			t.Fatal("stream ended before interrupt")
		}
		if ev.Type == "interrupt" {
			var payload struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Kind == "ask" {
				if err := postJSON(t, base+"/api/runs/"+runID+"/answer", `{"answers":["A"]}`); err != nil {
					t.Fatal(err)
				}
			} else if err := postJSON(t, base+"/api/runs/"+runID+"/answer", `{"approve":true}`); err != nil {
				t.Fatal(err)
			}
			break
		}
		if ev.Type == "done" || ev.Type == "error" {
			t.Fatalf("run ended before interrupt: %s %s", ev.Type, ev.Data)
		}
	}

	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		if ev.Type == "done" {
			return
		}
		if ev.Type == "error" {
			t.Fatalf("run error: %s", ev.Data)
		}
	}
}

func postJSON(t *testing.T, url, body string) error {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// readSSEEvent reads one complete SSE event (an `event:` line followed by a
// `data:` line and a blank line).
func readSSEEvent(sc *bufio.Scanner) (struct {
	Type string
	Data string
}, bool) {
	var ev struct {
		Type string
		Data string
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			ev.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "" && (ev.Type != "" || ev.Data != ""):
			return ev, true
		}
	}
	return ev, false
}

func TestQueueInputEndpoint(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Unknown run.
	resp, err := http.Post(ts.URL+"/api/runs/run-999/input", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run status = %d, want 404", resp.StatusCode)
	}

	// Create a run (it will interrupt on AskUserQuestion and park).
	body, _ := json.Marshal(map[string]string{"prompt": "ask me"})
	resp, err = http.Post(ts.URL+"/api/runs", "application/json", bytesReader(body))
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
	if created.RunID == "" {
		t.Fatal("no run id")
	}

	// Empty text is rejected.
	resp, err = http.Post(ts.URL+"/api/runs/"+created.RunID+"/input", "application/json", strings.NewReader(`{"text":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty text status = %d, want 400", resp.StatusCode)
	}

	// Valid input is queued.
	resp, err = http.Post(ts.URL+"/api/runs/"+created.RunID+"/input", "application/json", strings.NewReader(`{"text":"mid-turn note"}`))
	if err != nil {
		t.Fatal(err)
	}
	var queued struct {
		OK     bool `json:"ok"`
		Queued int  `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !queued.OK || queued.Queued != 1 {
		t.Fatalf("queue response = %+v", queued)
	}
}

func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }
