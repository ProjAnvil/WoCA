package agentbuild

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

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

func TestConnectMCPWithToken(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSecret("mcp_secure_token", "tok"); err != nil {
		t.Fatal(err)
	}
	// The stored token path builds an auth-capable HTTP client; the
	// unreachable endpoint fails initialization.
	got, clients := connectMCP(t.Context(), testLogger(), []string{"secure=http://127.0.0.1:1/mcp"}, st, nil)
	if len(got) != 0 || len(clients) != 0 {
		t.Fatalf("token connect = %d tools", len(got))
	}
}

func TestNewMcpAuthRunnerNonHTTPSpec(t *testing.T) {
	home := isolateEnv(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldArgs := os.Args
	os.Args = []string{"woca", "--mcp", "srv=echo hello"}
	defer func() { os.Args = oldArgs }()
	runner := McpAuthRunner(st)
	// The spec command is not an http(s) endpoint: configuration error.
	if err := runner(t.Context(), "srv"); err == nil {
		t.Fatal("expected not-configured error")
	}
}

func TestNewMcpAuthRunnerFullFlow(t *testing.T) {
	home := isolateEnv(t)
	stubBrowserOpen(t, true)
	auth := fakeAuthServerMux(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	oldArgs := os.Args
	os.Args = []string{"woca", "--mcp", "srv=" + auth + "/mcp"}
	defer func() { os.Args = oldArgs }()
	runner := McpAuthRunner(st)
	if err := runner(t.Context(), "srv"); err != nil {
		t.Fatal(err)
	}
	if !st.HasSecret("mcp_srv_token") {
		t.Fatal("token secret not stored")
	}
}

func TestConnectMCPRealServer(t *testing.T) {
	bin := buildMCPHelperServer(t)
	log := testLogger()

	specs := []string{"helper=" + bin}
	toolsGot, clients := connectMCP(t.Context(), log, specs, nil, nil)
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
	opts.MCPServers = specs
	ap, err := BuildAgent(t.Context(), opts, log)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.PluginMgr.Close()
	defer ap.Agent.Close()
}

func TestConnectMCPListToolsFailure(t *testing.T) {
	// The helper server initializes fine but fails tools/list on demand.
	bin := buildMCPHelperServer(t)
	t.Setenv("HELPER_FAIL_LIST", "1")
	got, clients := connectMCP(t.Context(), testLogger(), []string{"helper=" + bin}, nil, nil)
	if len(got) != 0 || len(clients) != 0 {
		t.Fatalf("failing list = %d tools %d clients", len(got), len(clients))
	}
}

func TestSyncMCPConfigBadClaudeJSON(t *testing.T) {
	home := isolateEnv(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncMCPConfig(filepath.Join(home, ".woca"), []string{"x"}); err == nil {
		t.Fatal("expected claude.json parse error")
	}
}

func TestSyncMCPConfigWriteFailure(t *testing.T) {
	home := isolateEnv(t)
	root := filepath.Join(home, ".woca")
	if err := os.MkdirAll(filepath.Join(root, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"a":{"command":"echo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncMCPConfig(root, []string{"a"}); err == nil {
		t.Fatal("expected write failure")
	}
}

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

func fakeAuthServerMux(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"client_id":"dyn-client-1"}`)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac-123&state="+r.URL.Query().Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("code_verifier") == "" || r.PostForm.Get("code") != "ac-123" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok-xyz","token_type":"bearer"}`)
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}
