package relayclient

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestRecorderFreezesAtFirstWrite(t *testing.T) {
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		// The ledger SSE path's awkward order (internal/server/runs.go):
		// a probe flush FIRST, the content type set only afterwards.
		// Freezing at flush would lose the content type and the SPA's
		// onopen check would reject the stream; freezing at first write
		// keeps it (design §6.3).
		rec.Flush()
		rec.Header().Set("Content-Type", "text/event-stream")
		rec.Header().Set("Content-Length", "123") // dropped from snapshots
		rec.Header().Set("Connection", "keep-alive")
		_, _ = rec.Write([]byte("data: x\n\n"))
		_ = pw.Close()
	}()
	<-rec.ready
	snap := rec.snapshot()
	if got := snap["Content-Type"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Fatalf("content type lost at snapshot: %v", snap)
	}
	if _, ok := snap["Content-Length"]; ok {
		t.Fatalf("Content-Length must be dropped from snapshots: %v", snap)
	}
	if !rec.isEventStream() {
		t.Fatal("isEventStream false for a text/event-stream snapshot")
	}
	if status := rec.committedStatus(); status != http.StatusOK {
		t.Fatalf("implicit status: %d, want 200", status)
	}
	body, _ := io.ReadAll(pr)
	if string(body) != "data: x\n\n" {
		t.Fatalf("body: %q", body)
	}
}

func TestRecorderExplicitStatusFreezesOnce(t *testing.T) {
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		rec.Header().Set("X-First", "1")
		rec.WriteHeader(http.StatusTeapot)
		rec.WriteHeader(http.StatusOK) // ignored: status froze at the first call
		_, _ = rec.Write([]byte("chunk"))
		_ = pw.Close()
	}()
	<-rec.ready
	if rec.committedStatus() != http.StatusTeapot {
		t.Fatalf("status: %d, want 418 (first WriteHeader wins)", rec.committedStatus())
	}
	if snap := rec.snapshot(); snap["X-First"][0] != "1" {
		t.Fatalf("snapshot: %v", snap)
	}
	_, _ = io.ReadAll(pr)
}

func TestRecorderEmptyResponse(t *testing.T) {
	// A handler that never writes still releases the uploader via finish.
	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		_ = pw.Close()
	}()
	select {
	case <-rec.ready:
	case <-time.After(time.Second):
		t.Fatal("finish did not release ready")
	}
	if rec.committedStatus() != http.StatusOK {
		t.Fatal("empty response must commit 200")
	}
	_, _ = io.ReadAll(pr)
}
