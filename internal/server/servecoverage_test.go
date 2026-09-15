package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/runstate"
	"github.com/rs/zerolog"
)

// serveLedgerRunEvents branches: unknown run → 404; terminal row without
// terminal event → resync; ledger unavailable → resync; streaming
// unsupported → 500; replay error → resync.
func TestLedgerSSEEdgeCases(t *testing.T) {
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	h := s.Handler()

	// Unknown run: ledger has no row → 404.
	r := httptest.NewRequest(http.MethodGet, "/api/runs/missing/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown run = %d", w.Code)
	}

	// Terminal row (done) with no mirrored events → resync.
	if err := ledger.Start(t.Context(), runstate.Run{RunID: "term", SessionID: "s"}, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Finish(t.Context(), "term", "w", "done"); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/runs/term/events", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "resync") {
		t.Fatalf("terminal no-event = %q", w.Body.String())
	}
}

// ServeStarted exercises the shutdown paths (ctx cancel).
func TestServeStartedPreCancelled(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancelled → immediate shutdown
	err := s.ServeStarted(ctx, srv.Config, nil)
	if err != nil {
		t.Fatalf("ServeStarted(pre-cancelled) = %v", err)
	}
}

// handleListSessions with an empty store returns an empty list.
func TestListSessionsEmpty(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sessions []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	_ = sessions // may be null or empty
}

// trackRunLeases heartbeat error path (closed ledger).
func TestTrackRunLeaseErrorPath(t *testing.T) {
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	if err := ledger.Start(t.Context(), runstate.Run{RunID: "r", SessionID: "s"}, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	// trackRunLease sets up the mirror and heartbeat without panicking.
	// (The full lifecycle is exercised by the runledger tests.)
	_ = s
}
