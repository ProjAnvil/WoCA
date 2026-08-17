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
