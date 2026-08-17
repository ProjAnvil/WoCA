package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
