package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/runstate"
)

// firstSSELine returns the first line of an SSE response body, failing the
// test when nothing arrives within 2s. Mirrors
// TestGlobalEventsWritesImmediately: the relay tunnel's streamRecorder
// commits response headers at the first Write (design §6.3), so a stream
// that stays silent until its first event hangs the tunneled response
// head — the browser's EventSource would sit in "connecting" forever.
func firstSSELine(t *testing.T, resp *http.Response) string {
	t.Helper()
	first := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		if sc.Scan() {
			first <- sc.Text()
		}
	}()
	select {
	case line := <-first:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("no initial frame within 2s — a tunneled stream cannot commit its response head")
		return ""
	}
}

func openRunEventsStream(t *testing.T, url string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	return resp
}

// TestRunEventsWritesImmediately covers the in-memory run SSE path
// (handleEvents): a freshly started run produces no event until the first
// model call, so the initial frame must be the SSE comment.
func TestRunEventsWritesImmediately(t *testing.T) {
	a := newScriptedAgent(t, "reply")
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	srv := httptest.NewServer(s.Handler())
	// t.Cleanup (not defer): the stream's cancel/close cleanups registered
	// later run first (LIFO), so Close never waits on an open SSE body.
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{"session_id": "sess-sse", "prompt": "hello"})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.RunID == "" {
		t.Fatal("no run id")
	}

	stream := openRunEventsStream(t, srv.URL+"/api/runs/"+out.RunID+"/events")
	if line := firstSSELine(t, stream); !strings.HasPrefix(line, ":") {
		t.Fatalf("first frame = %q, want an SSE comment", line)
	}
}

// TestLedgerRunEventsWritesImmediately covers the cross-instance ledger
// path (serveLedgerRunEvents): a foreign run with no mirrored events idles
// in the poll loop, so the initial frame must be the SSE comment.
func TestLedgerRunEventsWritesImmediately(t *testing.T) {
	a := newScriptedAgent(t, "unused")
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close) // see TestRunEventsWritesImmediately

	// A foreign run queued on another instance: no in-memory run here, no
	// mirrored events ever.
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: "foreign-sse", SessionID: "sess-foreign", ThreadID: "thread-foreign",
	}); err != nil {
		t.Fatal(err)
	}

	stream := openRunEventsStream(t, srv.URL+"/api/runs/foreign-sse/events")
	if line := firstSSELine(t, stream); !strings.HasPrefix(line, ":") {
		t.Fatalf("first frame = %q, want an SSE comment", line)
	}
}
