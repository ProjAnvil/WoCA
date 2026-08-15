package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStreamableHTTP(t *testing.T) {
	var (
		mu        sync.Mutex
		sessionID string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			mu.Lock()
			sessionID = sid
			mu.Unlock()
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"test"}}}`, req.ID)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			// A notification interleaved with the response.
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/foo\",\"params\":{\"x\":1}}\n\n")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"echo\",\"description\":\"d\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", req.ID)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"ok"}],"isError":false}}`, req.ID)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewStreamableHTTP("woca", srv.URL+"/mcp")
	var notified string
	client.OnNotification = func(method string, params json.RawMessage) { notified = method }

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
	if notified != "notifications/foo" {
		t.Fatalf("notification = %q", notified)
	}
	out, err := client.CallTool(context.Background(), "echo", map[string]any{"m": "x"})
	if err != nil || out != "ok" {
		t.Fatalf("call = %q err=%v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sessionID != "sess-123" {
		t.Fatalf("session id not threaded: %q", sessionID)
	}
}

func TestStreamableHTTPConcurrent(t *testing.T) {
	const N = 40

	// Barrier: every non-initialize handler signals arrival, then blocks until
	// all N requests are in flight. A transport that serializes round-trips
	// would only ever let one request reach the server, timing out below.
	arrived := make(chan struct{}, N)
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		// Thread the session id on every response to exercise the guarded
		// session field under concurrent round-trips.
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, req.ID)
		case "tools/list", "tools/call":
			// Barrier: block until all N requests are in flight, proving that
			// the transport issues requests concurrently rather than
			// serializing them.
			arrived <- struct{}{}
			<-release

			if req.Method == "tools/list" {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"tool-%d","description":"d"}]}}`, req.ID, req.ID)
			} else {
				var p struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(req.Params, &p)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"ok-%s"}],"isError":false}}`, req.ID, p.Name)
			}
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewStreamableHTTP("woca", srv.URL+"/mcp")
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		errs    = make([]error, N)
		listMu  sync.Mutex
		listGot []string
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				tools, err := client.ListTools(context.Background())
				if err != nil {
					errs[i] = err
					return
				}
				if len(tools) != 1 {
					errs[i] = fmt.Errorf("ListTools returned %d tools", len(tools))
					return
				}
				listMu.Lock()
				listGot = append(listGot, tools[0].Name)
				listMu.Unlock()
			} else {
				name := fmt.Sprintf("call-%d", i)
				out, err := client.CallTool(context.Background(), name, nil)
				if err != nil {
					errs[i] = err
					return
				}
				if want := "ok-" + name; out != want {
					errs[i] = fmt.Errorf("CallTool = %q, want %q", out, want)
				}
			}
		}(i)
	}

	// Wait for all N requests to arrive before releasing them.
	for i := 0; i < N; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("only %d/%d requests arrived concurrently (transport serialized?)", i, N)
		}
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	// Every ListTools response carries a distinct per-id tool name; duplicates
	// would indicate cross-talk between concurrent responses.
	seen := make(map[string]bool, len(listGot))
	for _, name := range listGot {
		if seen[name] {
			t.Fatalf("duplicate ListTools result %q", name)
		}
		seen[name] = true
	}
	if len(listGot) != N/2 {
		t.Fatalf("ListTools results = %d, want %d", len(listGot), N/2)
	}
}

func TestStreamableHTTPSSRF(t *testing.T) {
	// Private, link-local, and cloud-metadata hosts must be blocked.
	for _, endpoint := range []string{
		"http://169.254.169.254/",
		"http://10.0.0.1/mcp",
		"http://192.168.1.1/mcp",
	} {
		if c := NewStreamableHTTP("x", endpoint); c != nil {
			t.Fatalf("NewStreamableHTTP(%q) = %v, want nil", endpoint, c)
		}
	}

	// Loopback and public hosts are allowed (a user explicitly configures a
	// local MCP server; loopback is not an SSRF vector for explicit config).
	for _, endpoint := range []string{
		"http://127.0.0.1/mcp",
		"http://localhost/mcp",
		"https://example.com/mcp",
	} {
		if c := NewStreamableHTTP("x", endpoint); c == nil {
			t.Fatalf("NewStreamableHTTP(%q) returned nil, want non-nil", endpoint)
		}
	}
}
