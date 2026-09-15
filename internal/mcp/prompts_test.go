package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestListPromptsAndGetPrompt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, req.ID)
		case "prompts/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"prompts":[{"name":"greet","description":"greet someone","arguments":[{"name":"who","required":true}]},{"name":"summary"}]}}`, req.ID)
		case "prompts/get":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			who, _ := p.Arguments["who"].(string)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"messages":[{"role":"user","content":{"type":"text","text":"Hello, %s"}},{"role":"assistant","content":{"type":"text","text":"!"}}]}}`, req.ID, who)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewStreamableHTTP("woca", srv.URL+"/mcp")
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}

	prompts, err := client.ListPrompts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %+v", prompts)
	}
	if prompts[0].Name != "greet" || prompts[0].Description != "greet someone" {
		t.Fatalf("prompt[0] = %+v", prompts[0])
	}
	if len(prompts[0].Arguments) != 1 || prompts[0].Arguments[0].Name != "who" || !prompts[0].Arguments[0].Required {
		t.Fatalf("prompt[0].Arguments = %+v", prompts[0].Arguments)
	}
	if prompts[1].Name != "summary" {
		t.Fatalf("prompt[1] = %+v", prompts[1])
	}

	text, err := client.GetPrompt(t.Context(), "greet", map[string]any{"who": "world"})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello, world!" {
		t.Fatalf("text = %q", text)
	}
}

func TestAsPromptTools(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, req.ID)
		case "prompts/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"prompts":[{"name":"greet","description":"greet someone","arguments":[{"name":"who","required":true},{"name":"style"}]},{"name":"summary"}]}}`, req.ID)
		case "prompts/get":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			who, _ := p.Arguments["who"].(string)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"messages":[{"role":"user","content":{"type":"text","text":"Hello, %s"}}]}}`, req.ID, who)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewStreamableHTTP("woca", srv.URL+"/mcp")
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}

	tools := client.AsPromptTools("srv")
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	if tools[0].Name() != "mcp__srv__prompts" {
		t.Fatalf("list name = %q", tools[0].Name())
	}
	if tools[1].Name() != "mcp__srv__prompt" {
		t.Fatalf("get name = %q", tools[1].Name())
	}

	listRes, err := tools[0].Invoke(t.Context(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"greet", "greet someone", "who", "style", "summary"} {
		if !strings.Contains(listRes.Content, want) {
			t.Fatalf("list content = %q, missing %q", listRes.Content, want)
		}
	}

	getRes, err := tools[1].Invoke(t.Context(), map[string]any{
		"name":      "greet",
		"arguments": map[string]any{"who": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if getRes.Content != "Hello, world" {
		t.Fatalf("get content = %q", getRes.Content)
	}
}

func TestListen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			var req struct {
				ID int64 `json:"id"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, req.ID)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/resources/updated\",\"params\":{\"uri\":\"file:///a\"}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/resources/updated\",\"params\":{\"uri\":\"file:///b\"}}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewStreamableHTTP("woca", srv.URL+"/mcp")
	var (
		mu  sync.Mutex
		got []string
	)
	client.OnNotification = func(method string, params json.RawMessage) {
		mu.Lock()
		got = append(got, method)
		mu.Unlock()
	}
	if err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Listen(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("timed out waiting for notifications, got %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listen error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "notifications/resources/updated" || got[1] != "notifications/resources/updated" {
		t.Fatalf("notifications = %v", got)
	}
}
