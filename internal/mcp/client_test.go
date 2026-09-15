package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// startFakeServer wires an in-process JSON-RPC server over io.Pipe. It returns
// the client-side writer and reader.
func startFakeServer(t *testing.T, handler func(method string, params json.RawMessage) (any, error)) (io.Writer, io.Reader) {
	t.Helper()
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()

	go func() {
		defer serverR.Close()
		defer serverW.Close()
		sc := bufio.NewScanner(serverR)
		for sc.Scan() {
			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int64           `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			if req.Method == "notifications/initialized" {
				continue
			}
			result, err := handler(req.Method, req.Params)
			resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
			if err != nil {
				resp.Error = &rpcError{Code: -32600, Message: err.Error()}
			} else {
				raw, _ := json.Marshal(result)
				resp.Result = raw
			}
			data, _ := json.Marshal(resp)
			_, _ = serverW.Write(append(data, '\n'))
		}
	}()

	return clientW, clientR
}

func TestClientInitializeListAndCall(t *testing.T) {
	var calls []string
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "test"}}, nil
		case "tools/list":
			return map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "echo a message",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}},
			}}}, nil
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(params, &p)
			calls = append(calls, p.Name)
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "echoed: " + p.Arguments["message"].(string)}}, "isError": false}, nil
		default:
			return nil, &rpcError{Code: -32601, Message: "unknown method"}
		}
	})

	client := NewClient("woca", w, r, nil)
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}

	tools, err := client.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}

	out, err := client.CallTool(t.Context(), "echo", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "echoed: hi" {
		t.Fatalf("call output = %q", out)
	}
	if len(calls) != 1 || calls[0] != "echo" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestAsToolWrapsCallTool(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		if method == "tools/call" {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(t.Context())

	tool := client.AsTool("srv", Tool{Name: "echo", Description: "d", InputSchema: map[string]any{"type": "object"}})
	if tool.Name() != "mcp__srv__echo" {
		t.Fatalf("name = %q", tool.Name())
	}
	res, err := tool.Invoke(t.Context(), map[string]any{"message": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "ok") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestListAndReadResources(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{}, nil
		case "resources/list":
			return map[string]any{"resources": []map[string]any{{"uri": "file:///x", "name": "X", "description": "d"}}}, nil
		case "resources/read":
			return map[string]any{"contents": []map[string]any{{"uri": "file:///x", "text": "hello resource"}}}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(t.Context())

	resources, err := client.ListResources(t.Context())
	if err != nil || len(resources) != 1 || resources[0].Name != "X" {
		t.Fatalf("resources = %+v err=%v", resources, err)
	}
	text, err := client.ReadResource(t.Context(), "file:///x")
	if err != nil || text != "hello resource" {
		t.Fatalf("read = %q err=%v", text, err)
	}
}

func TestNotificationDispatch(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	client.OnNotification = func(method string, params json.RawMessage) {
		t.Logf("notification: %s %s", method, params)
	}
	_ = client.Initialize(t.Context())
}

func TestClientRequestErrorPaths(t *testing.T) {
	// The fake server rejects everything with an RPC error.
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		return nil, &rpcError{Code: -32000, Message: "boom"}
	})
	client := NewClient("err", w, r, nil)
	if err := client.Initialize(t.Context()); err == nil {
		t.Fatal("expected initialize error")
	}
	if _, err := client.ListTools(t.Context()); err == nil {
		t.Fatal("expected list error")
	}
	if _, err := client.CallTool(t.Context(), "echo", nil); err == nil {
		t.Fatal("expected call error")
	}
	if _, err := client.ListResources(t.Context()); err == nil {
		t.Fatal("expected resources error")
	}
	if _, err := client.ReadResource(t.Context(), "u"); err == nil {
		t.Fatal("expected read error")
	}
	if _, err := client.ListPrompts(t.Context()); err == nil {
		t.Fatal("expected prompts error")
	}
	if _, err := client.GetPrompt(t.Context(), "p", nil); err == nil {
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
	if err := c2.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.CallTool(t.Context(), "x", nil); err == nil {
		t.Fatal("expected isError to surface")
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
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTools(t.Context()); err == nil {
		t.Error("expected tools/list decode error")
	}
	if _, err := client.CallTool(t.Context(), "x", nil); err == nil {
		t.Error("expected tools/call decode error")
	}
	if _, err := client.ListResources(t.Context()); err == nil {
		t.Error("expected resources/list decode error")
	}
	if _, err := client.ReadResource(t.Context(), "u"); err == nil {
		t.Error("expected resources/read decode error")
	}
	if _, err := client.ListPrompts(t.Context()); err == nil {
		t.Error("expected prompts/list decode error")
	}
	if _, err := client.GetPrompt(t.Context(), "p", nil); err == nil {
		t.Error("expected prompts/get decode error")
	}
	if _, err := client.CreateMessage(t.Context(), CreateMessageParams{}); err == nil {
		t.Error("expected sampling decode error")
	}
	// Cancelled contexts fail before any request is written.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.ListTools(ctx); err == nil {
		t.Error("expected ctx error")
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
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := client.CallTool(t.Context(), "x", nil)
	if err != nil || out != "" {
		t.Fatalf("non-text content = %q %v (text parts only)", out, err)
	}
}
