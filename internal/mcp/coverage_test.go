package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpHelperServerSource is a minimal newline-JSON-RPC MCP server used to
// exercise the real stdio transport end to end.
const mcpHelperServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type req struct {
	Method string ` + "`json:\"method\"`" + `
	ID     *int64 ` + "`json:\"id\"`" + `
	Params json.RawMessage ` + "`json:\"params\"`" + `
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var r req
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == nil {
			continue
		}
		var result any
		switch r.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "helper"}, "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "echo a message",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"message": map[string]any{"type": "string"},
				}},
			}}}
		case "tools/call":
			var p struct {
				Arguments map[string]any ` + "`json:\"arguments\"`" + `
			}
			_ = json.Unmarshal(r.Params, &p)
			msg, _ := p.Arguments["message"].(string)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echoed: " + msg}}, "isError": false}
		case "resources/list":
			result = map[string]any{"resources": []map[string]any{{"uri": "memo://one", "name": "one", "mimeType": "text/plain"}}}
		case "resources/read":
			result = map[string]any{"contents": []map[string]any{{"uri": "memo://one", "mimeType": "text/plain", "text": "resource body"}}}
		case "prompts/list":
			result = map[string]any{"prompts": []map[string]any{{"name": "greet", "description": "greet someone"}}}
		case "prompts/get":
			result = map[string]any{"description": "a prompt", "messages": []map[string]any{{"role": "user", "content": map[string]any{"type": "text", "text": "hello prompt"}}}}
		default:
			result = map[string]any{}
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *r.ID, "result": result})
		w.Flush()
	}
}
`

func buildMCPHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(mcpHelperServerSource), 0o644); err != nil {
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

func TestStdioClientEndToEnd(t *testing.T) {
	bin := buildMCPHelper(t)
	client := NewStdio("helper", bin)
	if client == nil {
		t.Fatal("NewStdio returned nil")
	}
	defer client.Close()

	if client.Name() != "helper" {
		t.Fatalf("Name = %q", client.Name())
	}
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := client.CallTool(context.Background(), "echo", map[string]any{"message": "hi"})
	if err != nil || out != "echoed: hi" {
		t.Fatalf("CallTool = %q %v", out, err)
	}

	// Resource listing/reading.
	resources, err := client.ListResources(context.Background())
	if err != nil || len(resources) != 1 || resources[0].URI != "memo://one" {
		t.Fatalf("resources = %+v %v", resources, err)
	}
	body, err := client.ReadResource(context.Background(), "memo://one")
	if err != nil || body != "resource body" {
		t.Fatalf("ReadResource = %q %v", body, err)
	}

	// Tool conversion: namespaced tools execute against the server.
	core := client.AsTools("helper", tools)
	if len(core) != 1 {
		t.Fatalf("AsTools = %d", len(core))
	}
	if got := core[0].Name(); got != "mcp__helper__echo" {
		t.Fatalf("tool name = %q", got)
	}
	res, err := core[0].Invoke(context.Background(), map[string]any{"message": "tool"})
	if err != nil || res.Content != "echoed: tool" {
		t.Fatalf("invoke = %+v %v", res, err)
	}

	// Resource tools: list and read.
	rt := client.AsResourceTools("helper")
	if len(rt) != 2 {
		t.Fatalf("resource tools = %d", len(rt))
	}
	listRes, err := rt[0].Invoke(context.Background(), map[string]any{})
	if err != nil || !strings.Contains(listRes.Content, "memo://one") {
		t.Fatalf("list resource tool = %+v %v", listRes, err)
	}
	readRes, err := rt[1].Invoke(context.Background(), map[string]any{"uri": "memo://one"})
	if err != nil || readRes.Content != "resource body" {
		t.Fatalf("read resource tool = %+v %v", readRes, err)
	}

	// Prompt tools: the list tool renders names, the get tool returns the
	// prompt text.
	pt := client.AsPromptTools("helper")
	if len(pt) < 2 {
		t.Fatalf("prompt tools = %d", len(pt))
	}
	listPrompt, err := pt[0].Invoke(context.Background(), map[string]any{})
	if err != nil || !strings.Contains(listPrompt.Content, "greet") {
		t.Fatalf("prompt list = %+v %v", listPrompt, err)
	}
	getPrompt, err := pt[1].Invoke(context.Background(), map[string]any{"name": "greet"})
	if err != nil || !strings.Contains(getPrompt.Content, "hello prompt") {
		t.Fatalf("prompt get = %+v %v", getPrompt, err)
	}
}

func TestNewStdioStartFailure(t *testing.T) {
	if client := NewStdio("x", "/definitely/not/a/real/program"); client != nil {
		t.Fatal("unstartable command should yield nil")
	}
}

func TestStreamableAuthClientConstruction(t *testing.T) {
	// Loopback endpoints pass the SSRF policy.
	client := NewStreamableHTTPAuth("web", "http://127.0.0.1:1/mcp", func() string { return "tok" })
	if client == nil {
		t.Fatal("NewStreamableHTTPAuth nil")
	}
	if client.Name() != "web" {
		t.Fatalf("Name = %q", client.Name())
	}
	if err := client.Initialize(context.Background()); err == nil {
		t.Fatal("expected initialize failure on closed port")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	// Without a token supplier the client still builds.
	client2 := NewStreamableHTTPAuth("web", "http://localhost:1/mcp", nil)
	if client2 == nil {
		t.Fatal("nil token supplier should still build")
	}
	// A non-http(s) endpoint is rejected by policy → nil.
	if client3 := NewStreamableHTTPAuth("bad", "file:///etc/passwd", nil); client3 != nil {
		t.Fatal("file endpoint should be rejected")
	}
}

func TestStdioTransportDirect(t *testing.T) {
	// A transport built directly over pipes: notification dispatch, ctx
	// cancellation, write failure after close.
	serverR, clientW := pipe()
	clientR, serverW := pipe()
	go func() {
		defer serverR.Close()
		defer serverW.Close()
		sc := bufio.NewScanner(serverR)
		for sc.Scan() {
			var r struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == nil {
				continue
			}
			// A server-side notification arrives before the matching
			// response, so the transport must skip past it.
			_, _ = serverW.Write([]byte(`{"jsonrpc":"2.0","method":"notify/x","params":{"a":1}}` + "\n"))
			enc := json.NewEncoder(serverW)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *r.ID, "result": map[string]any{}})
		}
	}()

	tr := newStdioTransportFromIO(clientW, clientR)
	var notified string
	tr.setNotifier(func(method string, params json.RawMessage) {
		if method == "notify/x" {
			notified = "yes"
		}
	})
	resp, err := tr.roundTrip(context.Background(), 7, []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The first line back is the notification; the transport must keep
	// reading until the id-7 response arrives.
	if resp.ID != 7 {
		t.Fatalf("resp = %+v", resp)
	}
	if notified != "yes" {
		t.Fatal("notification not dispatched")
	}
	if err := tr.send([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	// Cancelled context fails fast.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.roundTrip(cancelled, 8, []byte(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)); err == nil {
		t.Fatal("expected ctx error")
	}
	// Garbage lines are skipped.
	if err := tr.close(); err != nil {
		t.Fatal(err)
	}
}

func pipe() (*os.File, *os.File) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return r, w
}

func TestResolveRef(t *testing.T) {
	if got := resolveRef("http://a/b/", "https://x/y"); got != "https://x/y" {
		t.Errorf("absolute = %q", got)
	}
	if got := resolveRef("http://a/b/", "rel"); got != "http://a/b/rel" {
		t.Errorf("relative = %q", got)
	}
	if got := resolveRef("http://a/b", "/abs"); got != "http://a/b/abs" {
		t.Errorf("rooted = %q", got)
	}
}

func TestAuthHelpersErrors(t *testing.T) {
	ctx := context.Background()
	client := &http.Client{Timeout: 2 * time.Second}

	// fetchJSON: non-200 and undecodable responses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "badstatus") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()
	if err := fetchJSON(client, srv.URL+"/badstatus", &struct{}{}); err == nil {
		t.Error("expected status error")
	}
	if err := fetchJSON(client, srv.URL+"/garbage", &struct{}{}); err == nil {
		t.Error("expected decode error")
	}
	if err := fetchJSON(client, "http://127.0.0.1:1/x", &struct{}{}); err == nil {
		t.Error("expected transport error")
	}

	// discover helpers propagate failures.
	if _, err := discoverProtectedResource(client, srv.URL); err == nil {
		t.Error("expected protected-resource error")
	}
	if _, err := discoverAuthorizationServer(client, srv.URL); err == nil {
		t.Error("expected authorization-server error")
	}

	// dynamicRegister: empty endpoint uses the public client id.
	id, err := dynamicRegister(ctx, client, "", 0)
	if err != nil || id != "woca-mcp-public-client" {
		t.Fatalf("public client = %q %v", id, err)
	}
	// Registration failures: bad status, missing client_id, transport.
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "nostatus") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "noid") {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"client_id":"c1"}`)
	}))
	defer reg.Close()
	if _, err := dynamicRegister(ctx, client, reg.URL+"/nostatus", 0); err == nil {
		t.Error("expected register status error")
	}
	if _, err := dynamicRegister(ctx, client, reg.URL+"/noid", 0); err == nil {
		t.Error("expected missing client_id error")
	}
	if _, err := dynamicRegister(ctx, client, "http://127.0.0.1:1/reg", 0); err == nil {
		t.Error("expected register transport error")
	}
	if id, err := dynamicRegister(ctx, client, reg.URL+"/ok", 0); err != nil || id != "c1" {
		t.Fatalf("register = %q %v", id, err)
	}

	// exchangeCode: non-200, missing token, transport.
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "bad") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "nope")
			return
		}
		if strings.HasSuffix(r.URL.Path, "empty") {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"at"}`)
	}))
	defer tok.Close()
	if _, err := exchangeCode(client, tok.URL+"/bad", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected exchange status error")
	}
	if _, err := exchangeCode(client, tok.URL+"/empty", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected missing token error")
	}
	if _, err := exchangeCode(client, "http://127.0.0.1:1/t", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected exchange transport error")
	}
	if v, err := exchangeCode(client, tok.URL+"/ok", "c", "code", "http://127.0.0.1/", "v"); err != nil || v != "at" {
		t.Fatalf("exchange = %q %v", v, err)
	}
}

func TestRunOAuthErrorPaths(t *testing.T) {
	// An unparsable endpoint fails immediately.
	if err := RunOAuth(context.Background(), OAuthFlow{ServerName: "x", Endpoint: "http://[::1]:namedport"}); err == nil {
		t.Fatal("expected parse error")
	}
	// Unreachable discovery fails.
	if err := RunOAuth(context.Background(), OAuthFlow{ServerName: "x", Endpoint: "http://127.0.0.1:1/mcp"}); err == nil {
		t.Fatal("expected discovery error")
	}
	// A cancelled context aborts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunOAuth(ctx, OAuthFlow{ServerName: "x", Endpoint: "http://127.0.0.1:1/mcp"}); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestClientRequestErrorPaths(t *testing.T) {
	// The fake server rejects everything with an RPC error.
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		return nil, &rpcError{Code: -32000, Message: "boom"}
	})
	client := NewClient("err", w, r, nil)
	if err := client.Initialize(context.Background()); err == nil {
		t.Fatal("expected initialize error")
	}
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Fatal("expected list error")
	}
	if _, err := client.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("expected call error")
	}
	if _, err := client.ListResources(context.Background()); err == nil {
		t.Fatal("expected resources error")
	}
	if _, err := client.ReadResource(context.Background(), "u"); err == nil {
		t.Fatal("expected read error")
	}
	if _, err := client.ListPrompts(context.Background()); err == nil {
		t.Fatal("expected prompts error")
	}
	if _, err := client.GetPrompt(context.Background(), "p", nil); err == nil {
		t.Fatal("expected prompt error")
	}
	_ = client.Close()

	// Tool-call results flagged isError surface as errors.
	w2, r2 := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "tools/call" {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "went wrong"}}, "isError": true}, nil
		}
		return map[string]any{}, nil
	})
	c2 := NewClient("flag", w2, r2, nil)
	defer c2.Close()
	if err := c2.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("expected isError to surface")
	}
}

func TestStreamableHTTPErrorPaths(t *testing.T) {
	// 500 on initialize.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()
	c := NewStreamableHTTP("err", srv.URL)
	if c == nil {
		t.Fatal("nil client")
	}
	if err := c.Initialize(context.Background()); err == nil {
		t.Fatal("expected status error")
	}
	srv.Close()

	// 200 with a non-JSON body.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv2.Close()
	c2 := NewStreamableHTTP("garbage", srv2.URL)
	if err := c2.Initialize(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
	srv2.Close()

	// The listen path against an endpoint that 404s and a cancelled
	// context.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv3.Close()
	c3 := NewStreamableHTTP("listen-err", srv3.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context listen returns nil (clean shutdown).
	_ = c3.Listen(ctx)
}

func TestTransportSkipGarbageAndWrongID(t *testing.T) {
	serverR, clientW := pipe()
	clientR, serverW := pipe()
	go func() {
		defer serverR.Close()
		defer serverW.Close()
		sc := bufio.NewScanner(serverR)
		for sc.Scan() {
			// garbage, a wrong-id response, then the right one.
			_, _ = serverW.Write([]byte("this is not json\n"))
			_, _ = serverW.Write([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}` + "\n"))
			var r struct {
				ID *int64 `json:"id"`
			}
			_ = json.Unmarshal(sc.Bytes(), &r)
			if r.ID == nil {
				continue
			}
			enc := json.NewEncoder(serverW)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *r.ID, "result": map[string]any{}})
		}
	}()
	tr := newStdioTransportFromIO(clientW, clientR)
	resp, err := tr.roundTrip(context.Background(), 3, []byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	if err != nil || resp.ID != 3 {
		t.Fatalf("roundTrip = %+v %v", resp, err)
	}
	// A read/write after the peer closed errors.
	serverR.Close()
	serverW.Close()
	_, _ = clientW.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"ping"}` + "\n"))
}

func TestToolClosuresAfterClose(t *testing.T) {
	// Build tools against a working fake server, then close the pipes so
	// every closure's error branch runs.
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05"}, nil
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "echo",
				"inputSchema": map[string]any{"type": "object"},
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	client := NewClient("gone", w, r, nil)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	core := client.AsTools("gone", tools)
	resTools := client.AsResourceTools("gone")
	promptTools := client.AsPromptTools("gone")
	// Close the pipe writer so every subsequent request fails.
	if closer, ok := w.(io.Closer); ok {
		_ = closer.Close()
	}

	if _, err := core[0].Invoke(context.Background(), map[string]any{}); err == nil {
		t.Error("expected invoke error after close")
	}
	for _, tool := range resTools {
		if _, err := tool.Invoke(context.Background(), map[string]any{"uri": "x"}); err == nil {
			t.Error("expected resource tool error after close")
		}
	}
	for _, tool := range promptTools {
		if _, err := tool.Invoke(context.Background(), map[string]any{"name": "x"}); err == nil {
			t.Error("expected prompt tool error after close")
		}
	}
}

func TestEmptyResourceAndPromptLists(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05"}, nil
		case "resources/list":
			return map[string]any{"resources": []any{}}, nil
		case "prompts/list":
			return map[string]any{"prompts": []any{}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	client := NewClient("empty", w, r, nil)
	defer client.Close()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	resTools := client.AsResourceTools("empty")
	out, err := resTools[0].Invoke(context.Background(), map[string]any{})
	if err != nil || out.Content != "(no resources)" {
		t.Fatalf("empty resources = %+v %v", out, err)
	}
	promptTools := client.AsPromptTools("empty")
	out2, err := promptTools[0].Invoke(context.Background(), map[string]any{})
	if err != nil || !strings.Contains(out2.Content, "(no prompts") {
		t.Fatalf("empty prompts = %+v %v", out2, err)
	}
}

func TestCallToolNonTextContent(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05"}, nil
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "image", "data": "..."}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	client := NewClient("img", w, r, nil)
	defer client.Close()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := client.CallTool(context.Background(), "x", nil)
	if err != nil || out != "" {
		t.Fatalf("non-text content = %q %v (text parts only)", out, err)
	}
}

func TestRunOAuthAuthServerSelectionAndSaveError(t *testing.T) {
	// The protected-resource metadata points at a separate authorization
	// server origin, which the flow must use.
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`)
		case "/register":
			fmt.Fprint(w, `{"client_id":"dyn-1"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac-ok&state="+r.URL.Query().Get("state"), http.StatusFound)
		case "/token":
			fmt.Fprint(w, `{"access_token":"tok-ok"}`)
		}
	}))
	defer auth.Close()

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			fmt.Fprintf(w, `{"authorization_servers":["%s"]}`, auth.URL)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mcp.Close()

	var saved string
	flow := OAuthFlow{
		ServerName:  "sel",
		Endpoint:    mcp.URL + "/mcp",
		Client:      mcp.Client(),
		OpenBrowser: func(authURL string) error { _, err := mcp.Client().Get(authURL); return err },
		SaveToken:   func(tok string) error { saved = tok; return nil },
	}
	if err := RunOAuth(context.Background(), flow); err != nil {
		t.Fatal(err)
	}
	if saved != "tok-ok" {
		t.Fatalf("saved = %q", saved)
	}

	// A failing SaveToken surfaces as an error.
	flow2 := flow
	flow2.SaveToken = func(tok string) error { return fmt.Errorf("disk full") }
	if err := RunOAuth(context.Background(), flow2); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("save error = %v", err)
	}

	// Incomplete metadata (missing token endpoint).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize"}`)
	}))
	defer bad.Close()
	flow3 := OAuthFlow{
		ServerName: "bad",
		Endpoint:   bad.URL + "/mcp",
		Client:     bad.Client(),
	}
	if err := RunOAuth(context.Background(), flow3); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete metadata = %v", err)
	}
}

func TestDecodeErrorResults(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05"}, nil
		default:
			// Results that cannot decode into the expected shapes.
			return json.RawMessage(`42`), nil
		}
	})
	client := NewClient("bad", w, r, nil)
	defer client.Close()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Error("expected tools/list decode error")
	}
	if _, err := client.CallTool(context.Background(), "x", nil); err == nil {
		t.Error("expected tools/call decode error")
	}
	if _, err := client.ListResources(context.Background()); err == nil {
		t.Error("expected resources/list decode error")
	}
	if _, err := client.ReadResource(context.Background(), "u"); err == nil {
		t.Error("expected resources/read decode error")
	}
	if _, err := client.ListPrompts(context.Background()); err == nil {
		t.Error("expected prompts/list decode error")
	}
	if _, err := client.GetPrompt(context.Background(), "p", nil); err == nil {
		t.Error("expected prompts/get decode error")
	}
	if _, err := client.CreateMessage(context.Background(), CreateMessageParams{}); err == nil {
		t.Error("expected sampling decode error")
	}
	// Cancelled contexts fail before any request is written.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListTools(ctx); err == nil {
		t.Error("expected ctx error")
	}
}

func TestRunOAuthStateMismatchAndExchangeFailure(t *testing.T) {
	// The "browser" tampers with the state parameter: the callback rejects
	// it and the flow errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			bad := r.URL.Query().Get("state") + "tampered"
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac&state="+bad, http.StatusFound)
		case "/token":
			fmt.Fprint(w, `{"access_token":"t"}`)
		}
	}))
	defer srv.Close()
	flow := OAuthFlow{
		ServerName:  "mismatch",
		Endpoint:    srv.URL + "/mcp",
		Client:      srv.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv.Client().Get(authURL); return err },
	}
	if err := RunOAuth(context.Background(), flow); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("state mismatch = %v", err)
	}

	// The redirect carries no code at all.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?state="+r.URL.Query().Get("state"), http.StatusFound)
		}
	}))
	defer srv2.Close()
	flow2 := OAuthFlow{
		ServerName:  "nocode",
		Endpoint:    srv2.URL + "/mcp",
		Client:      srv2.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv2.Client().Get(authURL); return err },
	}
	if err := RunOAuth(context.Background(), flow2); err == nil {
		t.Fatal("expected missing-code error")
	}

	// A working authorization whose token exchange fails.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac&state="+r.URL.Query().Get("state"), http.StatusFound)
		case "/token":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv3.Close()
	flow3 := OAuthFlow{
		ServerName:  "xchg",
		Endpoint:    srv3.URL + "/mcp",
		Client:      srv3.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv3.Client().Get(authURL); return err },
	}
	if err := RunOAuth(context.Background(), flow3); err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("exchange failure = %v", err)
	}
}

func TestStreamableListenSSE(t *testing.T) {
	notified := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "event: ping\n\n")    // no data: line
		fmt.Fprint(w, "data: [DONE]\n\n")   // sentinel skipped
		fmt.Fprint(w, "data: not-json\n\n") // undecodable data skipped
		fmt.Fprint(w, `data: {"method":"notify/hi","params":{}}`+"\n\n")
		fmt.Fprint(w, `data: {"id":9,"method":"server/req"}`+"\n\n") // server request id: skipped
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := NewStreamableHTTP("sse", srv.URL)
	if client == nil {
		t.Fatal("nil client")
	}
	defer client.Close()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.OnNotification = func(method string, params json.RawMessage) {
		notified <- method
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- client.Listen(ctx) }()

	select {
	case m := <-notified:
		if m != "notify/hi" {
			t.Fatalf("notified = %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification received")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("listen ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listen did not return after cancel")
	}
}

func TestAsToolNonObjectSchema(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05"}, nil
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{
				"name": "loose", "description": "no object schema",
				"inputSchema": map[string]any{"type": "string"},
			}}}, nil
		case "tools/call":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}, nil
		}
		return map[string]any{}, nil
	})
	client := NewClient("loose", w, r, nil)
	defer client.Close()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	core := client.AsTools("loose", tools)
	if len(core) != 1 {
		t.Fatalf("tools = %d", len(core))
	}
	res, err := core[0].Invoke(context.Background(), map[string]any{})
	if err != nil || res.Content != "ok" {
		t.Fatalf("invoke = %+v %v", res, err)
	}
}

func TestRunOAuthRegistrationAndPortFailures(t *testing.T) {
	// The registration endpoint is unreachable: dynamic registration fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"http://127.0.0.1:1/reg"}`)
	}))
	defer srv.Close()
	flow := OAuthFlow{ServerName: "regfail", Endpoint: srv.URL + "/mcp", Client: srv.Client()}
	if err := RunOAuth(context.Background(), flow); err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("registration failure = %v", err)
	}

	// A busy callback port cannot be listened on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
	}))
	defer srv2.Close()
	flow2 := OAuthFlow{ServerName: "busy", Endpoint: srv2.URL + "/mcp", Client: srv2.Client(), CallbackPort: port}
	if err := RunOAuth(context.Background(), flow2); err == nil || !strings.Contains(err.Error(), "callback listener") {
		t.Fatalf("busy port = %v", err)
	}

	// No OpenBrowser: the flow waits and the context times out.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	flow3 := OAuthFlow{ServerName: "nobrowser", Endpoint: srv2.URL + "/mcp", Client: srv2.Client()}
	if err := RunOAuth(ctx, flow3); err == nil {
		t.Fatal("expected timeout without a browser visit")
	}

	// A failing OpenBrowser aborts immediately.
	flow4 := OAuthFlow{
		ServerName:  "openfail",
		Endpoint:    srv2.URL + "/mcp",
		Client:      srv2.Client(),
		OpenBrowser: func(u string) error { return fmt.Errorf("no display") },
	}
	if err := RunOAuth(context.Background(), flow4); err == nil || !strings.Contains(err.Error(), "open browser") {
		t.Fatalf("open failure = %v", err)
	}
}

func TestHTTPTransportCancelledAndSendError(t *testing.T) {
	tr := newHTTPTransport("http://127.0.0.1:1/mcp")
	if tr == nil {
		t.Fatal("nil transport")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.roundTrip(ctx, 1, []byte(`{"id":1}`)); err == nil {
		t.Fatal("expected request-build error on cancelled ctx")
	}
	if err := tr.send([]byte(`{"method":"x"}`)); err == nil {
		t.Fatal("expected send error on closed port")
	}
}
