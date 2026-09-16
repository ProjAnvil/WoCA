// Package server exposes the woca coding agent over an HTTP API (SSE stream +
// JSON commands) and serves the embedded web frontend.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authn"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run := s.lookupRun(r.PathValue("id"))
	if run == nil {
		// The run may live on another instance sharing the database:
		// stream it from the run ledger instead (Phase 2 cross-instance
		// SSE).
		if s.ledger != nil && s.serveLedgerRunEvents(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The credential captured here is what the periodic revalidator
	// re-checks (Authorization header or the declared session cookie).
	cred := gateway.ExtractCredential(s.auth, r)
	// http.ResponseController flushes through middleware wrappers that
	// hide the underlying *http.Flusher (the observability RED middleware
	// wraps every writer); a direct type assertion would break SSE behind
	// it. The probe flush only happens after the headers are set so the
	// stream's content-type survives.
	flusher := http.NewResponseController(w)
	if err := flusher.Flush(); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Always emit an initial frame: the relay tunnel's streamRecorder
	// commits response headers at the first Write (design §6.3), so a run
	// with no events yet must still write — an SSE comment is a no-op for
	// EventSource but establishes the stream (and arms the heartbeat).
	fmt.Fprint(w, ": connected\n\n")
	_ = flusher.Flush()

	// Last-Event-ID carries the last seq the client saw; resuming replays
	// only the missed tail (or signals a gap outside the retention window).
	lastSeq, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	var sub *subscriber
	var gap bool
	if lastSeq > 0 {
		sub, gap = run.subscribeSince(lastSeq)
	} else {
		sub = run.subscribe()
	}
	defer run.unsubscribe(sub)
	if gap {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", EventResync, `{"reason":"history_gap"}`)
		_ = flusher.Flush()
	}

	// Revocation closing: open streams revalidate their credential, so
	// an administrator's revocation breaks active SSE within one
	// interval instead of lingering to the stream's natural end.
	var reauth <-chan time.Time
	if s.streamReauth != nil {
		t := time.NewTicker(streamReauthInterval)
		defer t.Stop()
		reauth = t.C
	}
	ctx := r.Context()
	for {
		select {
		case <-reauth:
			if err := s.streamReauth(cred); err != nil {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", EventAuthRevoked, `{"code":"`+authn.CodeOf(err)+`"}`)
				_ = flusher.Flush()
				return
			}
		case ev := <-sub.ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); err != nil {
				return
			}
			_ = flusher.Flush()
			if ev.Type == EventDone || ev.Type == EventError {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
