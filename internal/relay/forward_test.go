package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// failWriter errors on every write (streamChunks write-error path).
type failWriter struct{}

func (failWriter) Header() http.Header  { return http.Header{} }
func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("test: write failed")
}
func (failWriter) WriteHeader(int) {}

func TestStreamChunksWriteError(t *testing.T) {
	err := streamChunks(failWriter{}, nil, strings.NewReader("chunk"))
	if err == nil {
		t.Fatal("write failure must surface")
	}
}

// errAfterDataReader yields one chunk then a non-EOF read error.
type errAfterDataReader struct{ sent bool }

func (r *errAfterDataReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("test: read failed")
	}
	r.sent = true
	return copy(p, "chunk"), nil
}

func TestStreamChunksReadError(t *testing.T) {
	rec := &flushRecorder{header: http.Header{}}
	err := streamChunks(rec, rec, &errAfterDataReader{})
	if err == nil {
		t.Fatal("non-EOF read failure must surface")
	}
	if rec.body != "chunk" {
		t.Fatalf("partial stream lost: %q", rec.body)
	}
}

type flushRecorder struct {
	header http.Header
	body   string
	flush  int
}

func (r *flushRecorder) Header() http.Header { return r.header }
func (r *flushRecorder) WriteHeader(int)     {}
func (r *flushRecorder) Write(p []byte) (int, error) {
	r.body += string(p)
	return len(p), nil
}
func (r *flushRecorder) Flush() { r.flush++ }

func TestReadCappedBodyNil(t *testing.T) {
	// A nil body (handcrafted request) reads as empty, no error.
	body, err := readCappedBody(&flushRecorder{header: http.Header{}}, &http.Request{})
	if err != nil || body != nil {
		t.Fatalf("nil body: %v %v", body, err)
	}
}

func TestForwardBridgeBusyQueueFull(t *testing.T) {
	tr := newTestRelay(t)
	tr.claimGen(t, "g1")
	client := tr.login(t)

	// Fill the parking queue with unanswered client requests.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < ParkCapacity; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, tr.srv.URL+"/api/fill", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req) // blocks until cancel
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	waitForLen(t, tr, ParkCapacity)

	// The next request exceeds the per-key ceiling: 503 bridge busy.
	resp, err := client.Get(tr.srv.URL + "/api/overflow")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("overflow: %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"bridge_offline"`) {
		t.Fatalf("overflow body: %s", body)
	}

	// Client hangup mid-park drains the shell (the forward disconnect path).
	cancel()
	waitForLen(t, tr, 0)
	wg.Wait()
}

func waitForLen(t *testing.T, tr *testRelay, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for tr.rl.park.Len(tr.key.ID) != want {
		if time.Now().After(deadline) {
			t.Fatalf("parked len %d, want %d", tr.rl.park.Len(tr.key.ID), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
