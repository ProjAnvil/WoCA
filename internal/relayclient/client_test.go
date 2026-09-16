package relayclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	responds []capturedRespond
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	relay := newFakeRelay(t)
	relay.setPollCode(http.StatusGone)
	c := New(Config{URL: relay.srv.URL, Key: "wcr_test", Handler: http.NotFoundHandler(), Workers: 2}, testLogger())
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
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
	// Terminal means terminal: no more polls, ever (design §6.5).
	polls := relay.polls.Load()
	time.Sleep(200 * time.Millisecond)
	if relay.polls.Load() != polls {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, "repeat polls", func() bool { return relay.polls.Load() >= 3 })
	if st := c.Status(); !st.Connected || st.Revoked {
		t.Fatalf("status during idle polling: %+v", st)
	}
}

func TestBackoffBounds(t *testing.T) {
	for i := 0; i < 200; i++ {
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
