package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpenAIToolFlow replies to the first request with a streaming Write
// tool call and to every later request with plain text.
func fakeOpenAIToolFlow(t *testing.T, final string) string {
	t.Helper()
	var calls int32
	srv := newFakeStreamServer(t, func() []map[string]any {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"index": 0,
						"id":    "call_1",
						"type":  "function",
						"function": map[string]any{
							"name":      "Write",
							"arguments": `{"file_path":"note.txt","content":"written"}`,
						},
					}},
				},
				"finish_reason": nil,
			}, {
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "tool_calls",
			}}
		}
		return []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": final},
			"finish_reason": nil,
		}, {
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}}
	})
	return srv
}

// newFakeStreamServer starts a chat-completions SSE server driven by a
// per-request choice producer.
func newFakeStreamServer(t *testing.T, choices func() []map[string]any) string {
	t.Helper()
	srv := startHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, ch := range choices() {
			chunk := map[string]any{"id": "c1", "object": "chat.completion.chunk", "choices": []map[string]any{ch}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	return srv
}

func startHTTPServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// TestRunResumeCheckpointSuccess covers the --resume-checkpoint flow end to
// end: run one produces a persisted interrupt checkpoint (a Write permission
// request denied non-interactively), run two resumes it and approves.
func TestRunResumeCheckpointSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAIToolFlow(t, "resumed and approved")
	ckptDir := t.TempDir()

	// Run one: the model asks to write a file; non-interactive mode denies
	// and the turn completes, leaving the checkpoint file behind.
	opts := baseOpts(t, url)
	opts.prompt = "write a note"
	opts.checkpointDir = ckptDir
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(ckptDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no checkpoint persisted: %v (%d entries)", err, len(entries))
	}
	key := strings.TrimSuffix(entries[0].Name(), ".json")

	// Run two: resume the checkpoint, approve the write, JSON output. The
	// model only needs to produce the final text now.
	opts2 := baseOpts(t, fakeOpenAI(t, "resumed and approved"))
	opts2.checkpointDir = ckptDir
	opts2.resumeCheckpoint = key
	opts2.resumeValue = "y"
	opts2.outputFormat = "json"
	var out2, errBuf2 bytes.Buffer
	if err := run(t.Context(), opts2, strings.NewReader(""), &out2, &errBuf2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "resumed and approved") {
		t.Fatalf("resume output = %q", out2.String())
	}
}

func TestRunResumeLegacyCorrupt(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	// The first object decodes as an empty State; the second line is not a
	// message, so the legacy fallback load fails.
	if err := os.WriteFile(path, []byte("{}\nnot-json-at-all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected legacy resume error")
	}
}

const mcpHelperServer = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type req struct {
	Method string ` + "`json:\"method\"`" + `
	ID     *int64 ` + "`json:\"id\"`" + `
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var r req
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == nil {
			continue // notifications and garbage are ignored
		}
		var result any
		failed := false
		switch r.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "helper"}, "capabilities": map[string]any{}}
		case "tools/list":
			if os.Getenv("HELPER_FAIL_LIST") != "" {
				failed = true
			}
			result = map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "echo",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{}}
		default:
			result = map[string]any{}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": *r.ID}
		if failed {
			resp["error"] = map[string]any{"code": -32603, "message": "tools/list failed on demand"}
		} else {
			resp["result"] = result
		}
		enc.Encode(resp)
		w.Flush()
	}
}
`

// buildMCPHelperServer compiles the tiny stdio MCP server used to exercise
// the real connect path.
func buildMCPHelperServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(mcpHelperServer), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "mcphelper")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, out)
	}
	return bin
}

func TestConnectMCPRealServer(t *testing.T) {
	bin := buildMCPHelperServer(t)
	log := testLogger()

	specs := []string{"helper=" + bin}
	toolsGot, clients := connectMCP(t.Context(), log, specs, nil)
	if len(clients) != 1 {
		t.Fatalf("clients = %d", len(clients))
	}
	if len(toolsGot) == 0 {
		t.Fatal("expected tools from the helper server")
	}

	// The between-turn refresh re-lists tools successfully.
	refreshed := refreshMCPTools(t.Context(), log, clients)
	if len(refreshed) == 0 {
		t.Fatal("refresh produced no tools")
	}

	// A second connect through buildAgent wires the MCP tools into the agent.
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.mcpServers = specs
	ap, err := buildAgent(t.Context(), opts, log)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestServeAPIDrivesClosures(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	// An empty claude.json keeps the env apply path clean.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pick a free port, then hand it to serve().
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(ctx, opts, addr, true) }()

	base := "http://" + addr
	// serve() answers /api/health from the moment the port is bound; the
	// real handler (and the closures under test) only swaps in once the
	// agent build finishes, so wait for an API route instead.
	healthOK := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/api/quota")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthOK = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthOK {
		t.Fatal("server did not come up")
	}

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	put := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Closures wired in serve()/newConsole: quota, thinking, claude-env,
	// settings rebuilds, plugins.
	if resp := get("/api/quota"); resp.StatusCode != http.StatusOK {
		t.Fatalf("quota = %d", resp.StatusCode)
	}
	if resp := post("/api/agent/thinking", `{"budget":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("thinking = %d", resp.StatusCode)
	}
	if resp := put("/api/claudeenv", `{"skills":[],"mcp":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("claude-env = %d", resp.StatusCode)
	}
	if resp := put("/api/settings", `{"provider":"openai","memory_engine":"none"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", resp.StatusCode)
	}
	if resp := get("/api/plugins"); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins = %d", resp.StatusCode)
	}
	if resp := put("/api/plugins", `{"enabled":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins put = %d", resp.StatusCode)
	}
	// Installs of a real builtin plugin pass validation; the (offline)
	// install fails inside the plugin manager, executing both closure
	// branches.
	_ = post("/api/plugins/browser/install", "{}")
	_ = post("/api/plugins/browser/reinstall", "{}")

	// A run in a different workspace exercises the per-workspace hooks
	// closure wired in buildAgent.
	if resp := post("/api/runs", `{"prompt":"hi","workspace":`+quoteJSONString(t.TempDir())+`}`); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBuildAgentGitRepoAndPlugin(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	// A git repository workdir makes contextx.Collect produce output.
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A valid plugin contributing one skill and one agent.
	pluginRoot := filepath.Join(home, "plugins", "toolkit")
	skillDir := filepath.Join(pluginRoot, "skills", "deploy")
	agentDir := filepath.Join(pluginRoot, "agents")
	for _, d := range []string{skillDir, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `{"name":"toolkit","version":"1.0.0","description":"tools"}`
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: deploy\ndescription: deploy things\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "reviewer.md"), []byte("---\nname: reviewer\ndescription: reviews\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.workdir = ws
	ap, err := buildAgent(t.Context(), opts, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestLoginMkdirFailure(t *testing.T) {
	// HOME points at a regular file: credentials dir creation fails after a
	// successful device flow.
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"access_token":"at","refresh_token":"rt"}`,
	)
	t.Setenv("HOME", blocker)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected mkdir failure")
	}
}
