package relayclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testLogger() zerolog.Logger { return zerolog.Nop() }

// fakeRelay is the scriptable relay double: polls answer from scripted
// state, responds are captured for assertions, and a hijack-close mode
// simulates real transport failures (connection reset mid-poll).
type fakeRelay struct {
	t     *testing.T
	srv   *httptest.Server
	polls atomic.Int64

	hijackClose atomic.Bool

	mu       sync.Mutex
	payload  *pollPayload
	pollCode int
	// respondCode overrides the respond endpoint's 200 (410/404 rejection
	// tests).
	respondCode int
	responds    []capturedRespond
}

type capturedRespond struct {
	id      string
	status  string
	headers map[string][]string
	body    string
	gen     string
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel/poll":
			if f.hijackClose.Load() {
				// Kill the TCP connection without any HTTP response:
				// hc.Do sees a genuine transport error (unexpected EOF),
				// which is what regeneration keys off — an answered HTTP
				// 5xx deliberately does NOT rotate the generation (the
				// relay is alive and still holds this generation's 45s
				// lease; a fresh one would 409-collide with it).
				hj, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "hijack unsupported", http.StatusInternalServerError)
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			f.polls.Add(1)
			f.mu.Lock()
			code := f.pollCode
			p := f.payload
			f.payload = nil
			f.mu.Unlock()
			switch {
			case code != 0:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				if code == http.StatusGone {
					_, _ = w.Write([]byte(`{"code":"revoked"}`))
				}
			case p != nil:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(p)
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasPrefix(r.URL.Path, "/tunnel/respond/"):
			f.mu.Lock()
			code := f.respondCode
			f.mu.Unlock()
			if code != 0 {
				// Reject mid-stream like the real relay does for a
				// superseded generation: answer immediately and close the
				// connection, so the client's respond upload returns NOW
				// and its CloseWithError unsticks the handler's next pipe
				// write (design §6.5). A plain early 404 without the close
				// is absorbed by net/http until body EOF.
				hj, ok := w.(http.Hijacker)
				if !ok {
					w.WriteHeader(code)
					return
				}
				conn, bufrw, err := hj.Hijack()
				if err != nil {
					w.WriteHeader(code)
					return
				}
				_, _ = bufrw.Writer.WriteString(fmt.Sprintf("HTTP/1.1 %d Rejected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code))
				_ = bufrw.Flush()
				_ = conn.Close()
				return
			}
			body, _ := io.ReadAll(r.Body)
			var headers map[string][]string
			raw, _ := base64.StdEncoding.DecodeString(r.Header.Get(headersHeader))
			_ = json.Unmarshal(raw, &headers)
			f.mu.Lock()
			f.responds = append(f.responds, capturedRespond{
				id:      strings.TrimPrefix(r.URL.Path, "/tunnel/respond/"),
				status:  r.Header.Get(statusHeader),
				headers: headers,
				body:    string(body),
				gen:     r.Header.Get(generationHeader),
			})
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) offer(p pollPayload) {
	f.mu.Lock()
	f.payload = &p
	f.mu.Unlock()
}

func (f *fakeRelay) setPollCode(code int) {
	f.mu.Lock()
	f.pollCode = code
	f.mu.Unlock()
}

// setHijackClose toggles the transport-failure simulation.
func (f *fakeRelay) setHijackClose(on bool) {
	f.hijackClose.Store(on)
}

func (f *fakeRelay) respondCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.responds)
}

func (f *fakeRelay) lastRespond() capturedRespond {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responds) == 0 {
		f.t.Fatal("no respond captured")
	}
	return f.responds[len(f.responds)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestClientRoundTrip(t *testing.T) {
	relay := newFakeRelay(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Header hygiene (design §6.4): the forged signalling header is
		// stripped; the remote marker is injected by this process, after
		// the strip.
		if got := r.Header.Get("X-Woca-Relay-Generation"); got != "" {
			t.Errorf("signalling header reached the handler: %q", got)
		}
		if got := r.Header.Get(remoteHeader); got != "1" {
			t.Errorf("remote marker missing: %q", got)
		}
		if r.RemoteAddr != "203.0.113.7:5555" {
			t.Errorf("RemoteAddr %q (the real client IP must propagate)", r.RemoteAddr)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("ordinary header lost: Accept=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler, Workers: 1}, testLogger())
	ctx := t.Context()
	go c.Run(ctx)

	relay.offer(pollPayload{
		ID:     "req-1",
		Method: "GET",
		Path:   "/api/sessions",
		Client: "203.0.113.7:5555",
		Headers: map[string][]string{
			"X-Woca-Relay-Generation": {"forged"},
			"Accept":                  {"application/json"},
		},
	})
	waitFor(t, "respond upload", func() bool { return relay.respondCount() > 0 })
	got := relay.lastRespond()
	if got.id != "req-1" || got.status != "200" {
		t.Fatalf("respond: %+v", got)
	}
	if ct := got.headers["Content-Type"]; len(ct) != 1 || ct[0] != "application/json" {
		t.Fatalf("respond headers: %v", got.headers)
	}
	if !strings.Contains(got.body, `"path":"/api/sessions"`) {
		t.Fatalf("respond body: %q", got.body)
	}
	if got.gen == "" {
		t.Fatal("respond carried no generation header")
	}
}

func TestClientTerminalOnRevoked(t *testing.T) {
	var received, attempts atomic.Int64
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondFinished := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(releaseSecond) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch received.Add(1) {
		case 1:
			select {
			case <-secondStarted:
			case <-t.Context().Done():
				return
			}
		case 2:
			close(secondStarted)
			<-releaseSecond
			close(secondFinished)
		}
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"code":"revoked"}`)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
	c := New(Config{URL: srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 2}, testLogger())
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GetConn: func(string) { attempts.Add(1) },
	})
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("410 revoked did not terminate the tunnel")
	}
	st := c.Status()
	if !st.Revoked || st.LastError != "key revoked" {
		t.Fatalf("status: %+v (the status page reads 'key revoked')", st)
	}
	// An in-flight server handler can finish after cancellation. Observe
	// client request attempts instead of delayed server-side bookkeeping.
	if got := attempts.Load(); got != 2 {
		t.Fatalf("poll attempts = %d, want the two initial workers only", got)
	}
	unblock()
	select {
	case <-secondFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight server request did not finish")
	}
	if attempts.Load() != 2 {
		t.Fatal("terminal state kept polling")
	}
}

func TestClientTerminalOnUnauthorized(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusUnauthorized)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("401 did not terminate the tunnel")
	}
	if st := c.Status(); st.Revoked {
		t.Fatalf("401 is terminal but not 'revoked': %+v", st)
	}
}

func TestClientReconnectsWithNewGeneration(t *testing.T) {
	relay := newFakeRelay(t)
	// Regeneration is triggered by a REAL transport failure (connection
	// reset mid-poll), never by an answered HTTP 5xx: an answered 5xx
	// means the relay is alive and still holds this generation's 45s
	// lease, so rotating there would 409-collide the fresh generation
	// with the bridge's own leased one — up to 45s dark (design §6.2's
	// first-come-first-served rule). The implementation regenerates only
	// on hc.Do errors; this test therefore uses the hijack-close mode.
	relay.setHijackClose(true)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx := t.Context()
	go c.Run(ctx)
	first := c.generation()
	waitFor(t, "generation rotation", func() bool { return c.generation() != first })
	// Once the transport heals, the SAME (rotated) generation resumes
	// polling — retry, not terminal.
	relay.setHijackClose(false)
	waitFor(t, "recovery polls", func() bool { return relay.polls.Load() > 0 })
	if st := c.Status(); st.Revoked {
		t.Fatal("transport failure is retryable, not terminal")
	}
}

func TestClient5xxKeepsGeneration(t *testing.T) {
	relay := newFakeRelay(t)
	// An answered HTTP 500 is a plain retry: the relay is alive and holds
	// the lease — the generation must NOT rotate (it would 409-collide
	// with the bridge's own live generation for up to 45s).
	relay.setPollCode(http.StatusInternalServerError)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx := t.Context()
	go c.Run(ctx)
	first := c.generation()
	waitFor(t, "retry polls", func() bool { return relay.polls.Load() >= 2 })
	if got := c.generation(); got != first {
		t.Fatalf("generation rotated on HTTP 5xx (%s → %s); rotation is transport-error-only", first, got)
	}
	if st := c.Status(); st.Revoked {
		t.Fatal("5xx is retryable, not terminal")
	}
}

func TestClient204KeepsPolling(t *testing.T) {
	relay := newFakeRelay(t) // default: 204
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx := t.Context()
	go c.Run(ctx)
	waitFor(t, "repeat polls", func() bool { return relay.polls.Load() >= 3 })
	if st := c.Status(); !st.Connected || st.Revoked {
		t.Fatalf("status during idle polling: %+v", st)
	}
}

func TestBackoffBounds(t *testing.T) {
	for range 200 {
		if d := backoff(0); d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("attempt 0: %v outside ±20%% of 1s", d)
		}
		if d := backoff(100); d < 48*time.Second || d > 72*time.Second {
			t.Fatalf("capped: %v outside ±20%% of 60s", d)
		}
	}
	if d := backoff(3); d < 6*time.Second+400*time.Millisecond || d > 9*time.Second+600*time.Millisecond {
		t.Fatalf("attempt 3: %v outside ±20%% of 8s", d)
	}
}

func (f *fakeRelay) setRespondCode(code int) {
	f.mu.Lock()
	f.respondCode = code
	f.mu.Unlock()
}

func TestClientRunRequiresConfig(t *testing.T) {
	// No handler / URL / key: Run is an immediate no-op (serve wires it
	// unconditionally; the tunnel only runs when fully configured).
	for _, cfg := range []Config{
		{},
		{URL: "http://127.0.0.1:1", Key: "wcr_x"},
		{Handler: http.NotFoundHandler()},
	} {
		done := make(chan struct{})
		go func() {
			New(cfg, testLogger()).Run(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("unconfigured Run blocked: %+v", cfg)
		}
	}
}

func TestClientStatusKeyPrefixTruncation(t *testing.T) {
	c := New(Config{URL: "http://127.0.0.1:1", Key: "wcr_abcdefghijklmnop", Handler: http.NotFoundHandler()}, testLogger())
	st := c.Status()
	if st.KeyPrefix != "wcr_abcd" {
		t.Fatalf("key prefix %q, want the first 8 chars", st.KeyPrefix)
	}
	if !st.Configured {
		t.Fatal("configured tunnel reports unconfigured")
	}
	if st.Generation == "" {
		t.Fatal("New must mint the generation synchronously")
	}
}

func TestPollOnceInvalidURL(t *testing.T) {
	c := New(Config{URL: "http://bad\nurl", Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	// pollOnce outside Run: provide the termination context Run would set.
	c.term, c.termCancel = context.WithCancel(context.Background())
	defer c.termCancel()
	if got := c.pollOnce(context.Background()); got != outcomeTerminal {
		t.Fatalf("invalid URL: %v, want terminal", got)
	}
	if st := c.Status(); st.LastError != "invalid relay URL" {
		t.Fatalf("status: %+v", st)
	}
}

func TestPollOnceBare200EmptyBody(t *testing.T) {
	// A bare 200 with an empty body is the relay's mid-poll-hangup wake:
	// treated like 204 (immediate re-poll), never a decode error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := New(Config{URL: srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	if got := c.pollOnce(context.Background()); got != outcomeTimeout {
		t.Fatalf("bare 200: %v, want timeout-equivalent", got)
	}
	if st := c.Status(); st.LastError != "" {
		t.Fatalf("bare 200 recorded an error: %+v", st)
	}
}

func TestPollOnceDecodeErrorRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{truncated"))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{URL: srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	if got := c.pollOnce(context.Background()); got != outcomeRetry {
		t.Fatalf("undecodable 200: %v, want retry", got)
	}
	if st := c.Status(); !strings.Contains(st.LastError, "poll decode") {
		t.Fatalf("status: %+v", st)
	}
}

func TestPollOnceConflict(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusConflict)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	if got := c.pollOnce(context.Background()); got != outcomeConflict {
		t.Fatalf("409: %v, want conflict", got)
	}
	// A conflict is neither terminal nor an error on the status surface.
	if st := c.Status(); st.Revoked || st.LastError != "" {
		t.Fatalf("conflict status: %+v", st)
	}
}

func TestWorkerConflictBacksOffUntilCancel(t *testing.T) {
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusConflict)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 1}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	waitFor(t, "conflict polls", func() bool { return relay.polls.Load() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker stuck in conflict backoff after cancel")
	}
}

func TestRequestHopByHopConnectionNominees(t *testing.T) {
	hop := requestHopByHop(map[string][]string{
		"Connection": {"keep-alive, X-Trace"},
		"X-Trace":    {"abc"},
		"Accept":     {"text/plain"},
	})
	for _, name := range []string{"connection", "keep-alive", "x-trace", "te", "upgrade"} {
		if !hop[name] {
			t.Fatalf("hop nominee %q missing", name)
		}
	}
	if hop["accept"] {
		t.Fatal("ordinary header marked hop-by-hop")
	}
}

func TestExecuteDefaultsAndHopStrip(t *testing.T) {
	relay := newFakeRelay(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Client on the payload: the placeholder remote address.
		if r.RemoteAddr != "relay:0" {
			t.Errorf("RemoteAddr %q, want relay:0", r.RemoteAddr)
		}
		// The Connection nominee rides the tunnel but must not be
		// re-injected into the in-process request (design §6.4).
		if got := r.Header.Get("X-Trace"); got != "" {
			t.Errorf("connection-nominated header reached the handler: %q", got)
		}
		if got := r.Header.Get("Connection"); got != "" {
			t.Errorf("connection header reached the handler: %q", got)
		}
		_, _ = w.Write([]byte("ok"))
	})
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler}, testLogger())
	c.execute(context.Background(), pollPayload{
		ID:     "req-hop",
		Method: "GET",
		Path:   "/api/x",
		Headers: map[string][]string{
			"Connection": {"X-Trace"},
			"X-Trace":    {"forged"},
		},
	})
	waitFor(t, "respond upload", func() bool { return relay.respondCount() > 0 })
	if got := relay.lastRespond(); got.id != "req-hop" {
		t.Fatalf("respond: %+v", got)
	}
}

func TestExecutePanicsAreRecovered(t *testing.T) {
	relay := newFakeRelay(t)
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler}, testLogger())
	// A panicking tunneled handler must not take the worker down.
	c.execute(context.Background(), pollPayload{ID: "req-panic", Method: "GET", Path: "/api/x"})
}

func TestExecuteBadPayloadBody(t *testing.T) {
	c := New(Config{URL: "http://127.0.0.1:1", Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	c.execute(context.Background(), pollPayload{ID: "req-1", Method: "POST", Path: "/api/x", Body: "!!!not-base64"})
	if st := c.Status(); !strings.Contains(st.LastError, "payload body") {
		t.Fatalf("status: %+v", st)
	}
}

func TestExecuteBadMethod(t *testing.T) {
	c := New(Config{URL: "http://127.0.0.1:1", Key: "wcr_test", Handler: http.NotFoundHandler()}, testLogger())
	c.execute(context.Background(), pollPayload{ID: "req-1", Method: "BAD\nMETHOD", Path: "/api/x"})
	if st := c.Status(); !strings.Contains(st.LastError, "payload request") {
		t.Fatalf("status: %+v", st)
	}
}

func TestExecuteCancelBeforeReady(t *testing.T) {
	// The handler never writes: a cancel before the first byte releases
	// execute (shutdown during a hanging handler).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never writes; released by the cancel
	})
	c := New(Config{URL: "http://127.0.0.1:1", Key: "wcr_test", Handler: handler}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.execute(ctx, pollPayload{ID: "req-1", Method: "GET", Path: "/api/x"})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not release on cancel before ready")
	}
}

func TestRespondRejectedByRelay(t *testing.T) {
	relay := newFakeRelay(t)
	// The relay rejects the respond (superseded generation): the handler's
	// pipe writes fail fast instead of hanging (design §6.5).
	relay.setRespondCode(http.StatusNotFound)
	writeErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		for i := 0; i < 200 && err == nil; i++ {
			_, err = w.Write([]byte("x"))
			time.Sleep(10 * time.Millisecond)
		}
		writeErr <- err
	})
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler}, testLogger())
	c.execute(context.Background(), pollPayload{ID: "req-rej", Method: "GET", Path: "/api/x"})
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("handler kept writing after the relay rejected the respond")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler goroutine stuck after a rejected respond")
	}
}

func TestRespondTransportError(t *testing.T) {
	// The relay dies mid-respond: the error surfaces on the status page
	// and the handler's pipe is closed (design §6.5).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // dead before execute runs
	c := New(Config{URL: url, Key: "wcr_test", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
	})}, testLogger())
	c.execute(context.Background(), pollPayload{ID: "req-dead", Method: "GET", Path: "/api/x"})
	if st := c.Status(); !strings.Contains(st.LastError, "respond:") {
		t.Fatalf("status: %+v", st)
	}
}

func TestRespondInvalidRequestID(t *testing.T) {
	// A parked id that cannot form a request URL: respond aborts, the pipe
	// closes, the handler unblocks.
	relay := newFakeRelay(t)
	writeErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("first"))
		if err == nil {
			_, err = w.Write([]byte("second"))
		}
		writeErr <- err
	})
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: handler}, testLogger())
	c.execute(context.Background(), pollPayload{ID: "bad\nid", Method: "GET", Path: "/api/x"})
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("handler kept writing after the respond request failed to build")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler goroutine stuck after an unbuildable respond")
	}
}
