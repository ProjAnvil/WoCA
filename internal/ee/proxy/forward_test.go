// forward_test.go covers the data plane over real HTTP servers: streaming
// passthrough (no body buffering), bounded-queue shedding, queue timeout,
// client-disconnect queue release, tenant quota, create-body buffering and
// Authorization passthrough.
package proxy

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

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/wocadb"
)

// hit is one recorded backend request.
type hit struct {
	Method, Path, Auth, Workspace string
}

type hitLog struct {
	mu   sync.Mutex
	hits []hit
}

func (l *hitLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = append(l.hits, hit{
		Method:    r.Method,
		Path:      r.URL.Path,
		Auth:      r.Header.Get("Authorization"),
		Workspace: r.Header.Get(WorkspaceHeader),
	})
}

func (l *hitLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// ensureBinding writes a binding row directly (placement already covered).
func ensureBinding(t *testing.T, db *wocadb.DB, ws, tenant, executor string) {
	t.Helper()
	if _, err := db.EnsureWorkspaceBinding(wocadb.WorkspaceBinding{
		WorkspaceID: ws,
		SourceKey:   tenant + "|" + ws,
		TenantID:    tenant,
		ExecutorID:  executor,
	}); err != nil {
		t.Fatal(err)
	}
}

// newServedProxy mounts the proxy on a real server and returns both.
func newServedProxy(t *testing.T, db *wocadb.DB, l Limits) (*Proxy, string) {
	t.Helper()
	p := New(Config{DB: db, Limits: l, Logger: zerolog.Nop()})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return p, srv.URL
}

func doForward(ctx context.Context, client *http.Client, method, url, workspace, auth string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if workspace != "" {
		req.Header.Set(WorkspaceHeader, workspace)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

// blockedBackend answers 200 only after unblock. The unblock func is
// idempotent; register it as a t.Cleanup AFTER the proxy server is mounted
// so a failing assertion cannot strand blocked handlers ahead of the
// servers' Close cleanups (cleanups run in reverse registration order).
func blockedBackend(t *testing.T) (addr string, unblock func()) {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	return srv.URL, func() { once.Do(func() { close(block) }) }
}

// TestForwardAuthorizationPassthrough: the Authorization header reaches the
// executor verbatim; without it the proxy refuses before forwarding.
func TestForwardAuthorizationPassthrough(t *testing.T) {
	db := openTestDB(t)
	var log hitLog
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)
	seedAck(t, db, "exec-1", backend.URL, 0, 0)
	_, url := newServedProxy(t, db, Limits{})
	ensureBinding(t, db, "ws_auth", "t1", "exec-1")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_auth/exec", "ws_auth", "", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing Authorization: status %d, want 401", resp.StatusCode)
	}

	resp, err = doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_auth/exec", "ws_auth", "Bearer executor-secret", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized forward: status %d, want 200", resp.StatusCode)
	}
	waitFor(t, "backend hit recorded", func() bool { return log.count() == 1 })
	log.mu.Lock()
	got := log.hits[0]
	log.mu.Unlock()
	if got.Auth != "Bearer executor-secret" {
		t.Fatalf("Authorization at backend = %q, want passthrough", got.Auth)
	}
	if got.Path != "/sandbox/ws_auth/exec" || got.Workspace != "ws_auth" {
		t.Fatalf("backend saw path=%q workspace=%q, want unchanged", got.Path, got.Workspace)
	}
}

// TestForwardBackendUnreachable: a dead executor address surfaces as 502,
// not a hang.
func TestForwardBackendUnreachable(t *testing.T) {
	db := openTestDB(t)
	seedAck(t, db, "exec-1", "http://127.0.0.1:1", 0, 0) // nothing listens
	_, url := newServedProxy(t, db, Limits{})
	ensureBinding(t, db, "ws_dead", "t1", "exec-1")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_dead/exec", "ws_dead", "Bearer k", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

// TestCreateBodyBufferedAndRouted: the create body is small JSON — the proxy
// buffers it to find the workspace field when the header is absent, and
// resets the body so the executor receives it intact. The header wins over
// the body field. Oversized and malformed create bodies are rejected.
func TestCreateBodyBufferedAndRouted(t *testing.T) {
	db := openTestDB(t)
	var (
		mu          sync.Mutex
		seenBodies  []map[string]any
		backendHits = map[string]int{}
	)
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seenBodies = append(seenBodies, body)
		backendHits["a"]++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sandbox_id":"sbx_1"}`))
	}))
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seenBodies = append(seenBodies, body)
		backendHits["b"]++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sandbox_id":"sbx_1"}`))
	}))
	t.Cleanup(backendA.Close)
	t.Cleanup(backendB.Close)
	seedAck(t, db, "exec-1", backendA.URL, 0, 0)
	seedAck(t, db, "exec-2", backendB.URL, 0, 0)
	_, url := newServedProxy(t, db, Limits{})
	ensureBinding(t, db, "ws_body", "t1", "exec-1")
	ensureBinding(t, db, "ws_hdr", "t2", "exec-2")

	client := &http.Client{Timeout: 5 * time.Second}

	// No header: the body's workspace field routes the request.
	resp, err := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/create", "", "Bearer k", strings.NewReader(`{"workspace":"ws_body","cwd":"/workspace"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create via body: status %d", resp.StatusCode)
	}

	// Header present: it wins over the body field.
	resp, err = doForward(context.Background(), client, http.MethodPost, url+"/sandbox/create", "ws_hdr", "Bearer k", strings.NewReader(`{"workspace":"ws_body","cwd":"/workspace"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create via header: status %d", resp.StatusCode)
	}

	mu.Lock()
	hitsA, hitsB := backendHits["a"], backendHits["b"]
	bodies := append([]map[string]any(nil), seenBodies...)
	mu.Unlock()
	if hitsA != 1 || hitsB != 1 {
		t.Fatalf("backend hits a=%d b=%d, want 1 each (body-routed then header-routed)", hitsA, hitsB)
	}
	for i, b := range bodies {
		if b["cwd"] != "/workspace" {
			t.Fatalf("create body #%d reached executor mangled: %v", i, b)
		}
	}

	// Oversized create body → 413 (or a torn connection when the client is
	// mid-upload; both mean rejection).
	resp, err = doForward(context.Background(), client, http.MethodPost, url+"/sandbox/create", "ws_body", "Bearer k",
		strings.NewReader(`{"workspace":"ws_body","pad":"`+strings.Repeat("x", createBodyLimit+64)+`"}`))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized create: status %d, want 413", resp.StatusCode)
		}
	}

	// Malformed create JSON → 400.
	resp, err = doForward(context.Background(), client, http.MethodPost, url+"/sandbox/create", "", "Bearer k", strings.NewReader(`{"workspace":`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed create: status %d, want 400", resp.StatusCode)
	}

	// Empty create body without a header → missing-workspace 503.
	resp, err = doForward(context.Background(), client, http.MethodPost, url+"/sandbox/create", "", "Bearer k", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("empty create without header: status %d, want 503", resp.StatusCode)
	}
}

// TestStreamForwardingDoesNotBuffer: a tar.gz upload (PUT archive) streams
// chunk-by-chunk — the backend receives chunk N while the client is still
// writing N+1. A body-buffering proxy deadlocks the barrier and the test
// fails on the timeout.
func TestStreamForwardingDoesNotBuffer(t *testing.T) {
	db := openTestDB(t)
	const (
		chunkSize = 128 << 10
		chunks    = 8
	)
	var received int64
	gotChunk := make(chan struct{}, chunks)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, chunkSize)
		for i := 0; i < chunks; i++ {
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			received += chunkSize
			gotChunk <- struct{}{}
		}
		_ = r.Body.Close()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"received":%d}`, received)))
	}))
	t.Cleanup(backend.Close)
	seedAck(t, db, "exec-1", backend.URL, 0, 0)
	_, url := newServedProxy(t, db, Limits{})
	ensureBinding(t, db, "ws_stream", "t1", "exec-1")

	pr, pw := io.Pipe()
	respCh := make(chan *http.Response, 1)
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest(http.MethodPut, url+"/sandbox/ws_stream/archive", pr)
	req.Header.Set(WorkspaceHeader, "ws_stream")
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/gzip")
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			respCh <- nil
			return
		}
		respCh <- resp
	}()

	chunk := make([]byte, chunkSize)
	for i := 0; i < chunks; i++ {
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
		select {
		case <-gotChunk:
		case <-time.After(5 * time.Second):
			t.Fatalf("request body buffered: backend did not see chunk %d while the client was still writing", i+1)
		}
	}
	_ = pw.Close()

	select {
	case resp := <-respCh:
		if resp == nil {
			t.Fatal("streaming upload failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("streaming upload status %d", resp.StatusCode)
		}
		var out struct {
			Received int64 `json:"received"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.Received != chunkSize*chunks {
			t.Fatalf("backend received %d bytes, want %d", out.Received, chunkSize*chunks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streaming upload response never arrived")
	}
}

// TestStreamResponseNotBuffered: the download direction (GET archive)
// flushes chunk-by-chunk — the client reads chunk 1 while the backend is
// still writing chunk 2.
func TestStreamResponseNotBuffered(t *testing.T) {
	db := openTestDB(t)
	const (
		chunkSize = 64 << 10
		chunks    = 4
	)
	clientRead := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		chunk := make([]byte, chunkSize)
		for i := 0; i < chunks; i++ {
			if i == 1 {
				// Chunk 0 is out; hold the rest until the client has read it.
				select {
				case <-clientRead:
				case <-r.Context().Done():
					return
				}
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}))
	t.Cleanup(backend.Close)
	seedAck(t, db, "exec-1", backend.URL, 0, 0)
	_, url := newServedProxy(t, db, Limits{})
	ensureBinding(t, db, "ws_dl", "t1", "exec-1")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := doForward(context.Background(), client, http.MethodGet, url+"/sandbox/ws_dl/archive", "ws_dl", "Bearer k", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make([]byte, chunkSize)
	type readRes struct {
		n   int
		err error
	}
	firstCh := make(chan readRes, 1)
	go func() {
		n, err := io.ReadFull(resp.Body, first)
		firstCh <- readRes{n, err}
	}()
	select {
	case r := <-firstCh:
		if r.err != nil {
			t.Fatalf("first response chunk: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response buffered: first chunk did not arrive while the backend was mid-stream")
	}
	close(clientRead)
	rest, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if rest != chunkSize*(chunks-1) {
		t.Fatalf("downloaded %d trailing bytes, want %d", rest, chunkSize*(chunks-1))
	}
}

// TestQueueFullSheds503: concurrency 1 + queue 1 — the third request sheds
// with 503 while the first two eventually complete.
func TestQueueFullSheds503(t *testing.T) {
	db := openTestDB(t)
	backendAddr, unblock := blockedBackend(t)
	seedAck(t, db, "exec-1", backendAddr, 0, 0)
	ensureBinding(t, db, "ws_q1", "t1", "exec-1")
	p, url := newServedProxy(t, db, Limits{PerExecutor: 1, QueueLen: 1, TenantQuota: 8, QueueTimeout: 20 * time.Second})
	t.Cleanup(unblock)
	client := &http.Client{Timeout: 20 * time.Second}

	go func() {
		resp, _ := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_q1/exec", "ws_q1", "Bearer k", strings.NewReader("{}"))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "request 1 occupies the executor", func() bool { return p.queues.get("exec-1").inUse() == 1 })
	go func() {
		resp, _ := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_q1/exec", "ws_q1", "Bearer k", strings.NewReader("{}"))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "request 2 queues", func() bool { return p.queues.get("exec-1").depth("t1") == 1 })

	resp, err := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_q1/exec", "ws_q1", "Bearer k", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third request status %d, want 503 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "queue full") {
		t.Fatalf("503 body %q lacks queue-full reason", body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("queue-full 503 without Retry-After")
	}

	unblock()
	waitFor(t, "executor drains", func() bool { return p.queues.get("exec-1").inUse() == 0 })
}

// TestQueueTimeout503: a queued request outstays QueueTimeout → 503 naming
// the wait, while the in-flight request finishes normally.
func TestQueueTimeout503(t *testing.T) {
	db := openTestDB(t)
	backendAddr, unblock := blockedBackend(t)
	seedAck(t, db, "exec-1", backendAddr, 0, 0)
	ensureBinding(t, db, "ws_t1", "t1", "exec-1")
	p, url := newServedProxy(t, db, Limits{PerExecutor: 1, QueueLen: 8, TenantQuota: 8, QueueTimeout: 150 * time.Millisecond})
	t.Cleanup(unblock)
	client := &http.Client{Timeout: 20 * time.Second}

	go func() {
		resp, _ := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_t1/exec", "ws_t1", "Bearer k", strings.NewReader("{}"))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "request 1 occupies the executor", func() bool { return p.queuesFor(t, "exec-1").inUse() == 1 })

	start := time.Now()
	resp, err := doForward(context.Background(), client, http.MethodPost, url+"/sandbox/ws_t1/exec", "ws_t1", "Bearer k", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "queue wait") {
		t.Fatalf("queued request: code=%d body=%s, want 503 queue wait", resp.StatusCode, body)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("queue timeout took %s, want ~QueueTimeout", time.Since(start))
	}

	unblock()
	waitFor(t, "in-flight request completes", func() bool { return p.queuesFor(t, "exec-1").inUse() == 0 })
}

// queuesFor is queues.get with the test's fatal semantics.
func (p *Proxy) queuesFor(t *testing.T, executor string) *execQueue {
	t.Helper()
	q := p.queues.get(executor)
	if q == nil {
		t.Fatalf("no queue for %s", executor)
	}
	return q
}

// TestClientDisconnectReleasesQueueSlot: a cancelled queued request gives
// its queue position back — the tenant's quota is not leaked, the executor
// drains, and a foreign tenant queues unaffected. The disconnecting request
// is body-less (GET): net/http only arms its connection-close detection
// once a request body is consumed, so a body-carrying queued request would
// only reclaim via the queue timeout (a bounded, documented behavior).
func TestClientDisconnectReleasesQueueSlot(t *testing.T) {
	db := openTestDB(t)
	backendAddr, unblock := blockedBackend(t)
	seedAck(t, db, "exec-1", backendAddr, 0, 0)
	ensureBinding(t, db, "ws_da", "tenant-a", "exec-1")
	ensureBinding(t, db, "ws_db", "tenant-b", "exec-1")
	p, url := newServedProxy(t, db, Limits{PerExecutor: 1, QueueLen: 8, TenantQuota: 2, QueueTimeout: 20 * time.Second})
	t.Cleanup(unblock)
	client := &http.Client{Timeout: 20 * time.Second}
	exec := func() *execQueue { return p.queuesFor(t, "exec-1") }

	// In-flight request holds the executor.
	go func() {
		resp, _ := doForward(context.Background(), client, http.MethodPut, url+"/sandbox/ws_da/archive", "ws_da", "Bearer k", strings.NewReader("r1"))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "in-flight request occupies the executor", func() bool { return exec().inUse() == 1 })

	// A queued request whose client disconnects must release its position.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		resp, _ := doForward(ctx, client, http.MethodGet, url+"/sandbox/ws_da/archive", "ws_da", "Bearer k", nil)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "request 2 queues", func() bool { return exec().depth("tenant-a") == 1 })
	cancel()
	waitFor(t, "cancelled request releases its queue position", func() bool { return exec().depth("tenant-a") == 0 })

	// The tenant can still fill its full quota (2) — no leak from r2.
	for i := 3; i <= 4; i++ {
		go func(i int) {
			resp, _ := doForward(context.Background(), client, http.MethodPut, fmt.Sprintf("%s/sandbox/ws_da/archive?r=%d", url, i), "ws_da", "Bearer k", strings.NewReader("x"))
			if resp != nil {
				resp.Body.Close()
			}
		}(i)
	}
	waitFor(t, "tenant-a fills its quota", func() bool { return exec().depth("tenant-a") == 2 })

	// Beyond the quota: rejected 503 for the same tenant…
	resp, err := doForward(context.Background(), client, http.MethodPut, url+"/sandbox/ws_da/archive", "ws_da", "Bearer k", strings.NewReader("r5"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "tenant queue quota") {
		t.Fatalf("over-quota request: code=%d body=%s, want 503 tenant quota", resp.StatusCode, body)
	}

	// …while the foreign tenant still queues on the same executor.
	go func() {
		resp, _ := doForward(context.Background(), client, http.MethodPut, url+"/sandbox/ws_db/archive", "ws_db", "Bearer k", strings.NewReader("r6"))
		if resp != nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "tenant-b queues past tenant-a's full quota", func() bool { return exec().depth("tenant-b") == 1 })

	unblock()
	waitFor(t, "executor fully drains", func() bool {
		return exec().inUse() == 0 && exec().depth("tenant-a") == 0 && exec().depth("tenant-b") == 0
	})
}
