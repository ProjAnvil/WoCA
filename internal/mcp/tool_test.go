package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

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
