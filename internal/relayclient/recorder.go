package relayclient

import (
	"io"
	"net/http"
	"strings"
	"sync"
)

// streamRecorder captures a handler's response as a stream (design §6.3;
// a same-semantics rewrite of the enterprise streamRecorder — code in
// internal/ee/bridge/tunnel.go:273-336, NOT importable): status and
// headers freeze at the first Write/WriteHeader — exactly when net/http
// would commit them — NOT at Flush, because woca's ledger SSE path sets
// Content-Type: text/event-stream only after a probe flush; freezing at
// flush would drop the content type and the SPA's onopen check would
// reject the stream. Flush is a no-op boundary hint: every pipe write
// already becomes one transport chunk.
type streamRecorder struct {
	pw     io.Writer
	header http.Header
	status int

	mu        sync.Mutex
	ready     chan struct{}
	markReady func()
}

func newStreamRecorder(pw io.Writer) *streamRecorder {
	r := &streamRecorder{pw: pw, header: http.Header{}, ready: make(chan struct{})}
	r.markReady = sync.OnceFunc(func() { close(r.ready) })
	return r
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	if r.status == 0 {
		r.status = code
	}
	r.mu.Unlock()
	r.markReady()
}

func (r *streamRecorder) Write(b []byte) (int, error) {
	r.markReady()
	return r.pw.Write(b)
}

// Flush is a no-op: pipe writes already chunk the stream (design §6.3).
func (r *streamRecorder) Flush() {}

// finish releases waiters when the handler returns without writing.
func (r *streamRecorder) finish() { r.markReady() }

// snapshot copies the frozen headers, dropping Content-Length — a
// streamed answer has no fixed length (design §6.3).
func (r *streamRecorder) snapshot() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.header))
	for k, vs := range r.header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// committedStatus returns the frozen status (implicit 200).
func (r *streamRecorder) committedStatus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// isEventStream reports whether the frozen headers declare SSE (the
// heartbeat wraps only these).
func (r *streamRecorder) isEventStream() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream")
}
