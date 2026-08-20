package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func openRunLedger(t *testing.T) *runstate.Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	l, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// startAndAwaitRun drives POST /api/runs and waits for the run's ledger row
// to reach the wanted status.
func startAndAwaitRun(t *testing.T, s *Server, srv *httptest.Server, ledger *runstate.Ledger, sessionID, prompt string, wantStatus string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"session_id": sessionID, "prompt": prompt})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RunID == "" {
		t.Fatal("no run_id in response")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		row, ok, err := ledger.Row(context.Background(), out.RunID)
		if err == nil && ok && row.Status == wantStatus && (wantStatus != "done" || row.ThreadID != "") {
			return out.RunID
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, ok, _ := ledger.Row(context.Background(), out.RunID)
	s.mu.Lock()
	r := s.runs[out.RunID]
	s.mu.Unlock()
	kind := "(none)"
	if r != nil {
		kind = r.finishType()
		if kind == "" {
			kind = "(running)"
		}
	}
	t.Fatalf("run %s never reached status %q (ledger=%+v ok=%v in-memory finish=%q)", out.RunID, wantStatus, row, ok, kind)
	return ""
}

func TestRunLedgerRecordsRunLifecycle(t *testing.T) {
	a := newScriptedAgent(t, "ledger-reply")
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	runID := startAndAwaitRun(t, s, srv, ledger, "sess-ledger-1", "hello ledger", "done")

	row, ok, err := ledger.Row(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("ledger row = ok=%v err=%v", ok, err)
	}
	status, thread, owner := row.Status, row.ThreadID, row.Owner
	if status != "done" {
		t.Fatalf("terminal status = %q", status)
	}
	if thread == "" {
		t.Fatal("ledger row has no checkpoint thread id")
	}
	if owner != s.workerID {
		t.Fatalf("owner = %q, want %q", owner, s.workerID)
	}
	// Terminal rows stop holding the per-session slot: a new run for the
	// same session enqueues cleanly.
	startAndAwaitRun(t, s, srv, ledger, "sess-ledger-1", "second", "done")
}

func TestRunLedgerRejectsCrossInstanceActiveRun(t *testing.T) {
	a := newScriptedAgent(t, "busy-reply")
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Simulate another instance holding the session: a foreign active row.
	if err := ledger.Enqueue(context.Background(), runstate.Run{
		RunID: "foreign-run", SessionID: "sess-foreign", ThreadID: "thread-foreign",
	}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"session_id": "sess-foreign", "prompt": "hi"})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.RunID == "" {
		t.Fatal("run id missing")
	}
	// The run must terminate with the cross-instance error, not execute.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		r := s.runs[out.RunID]
		s.mu.Unlock()
		if r != nil && r.isDone() {
			if got := r.finishType(); got != "error" {
				t.Fatalf("terminal event = %q, want error", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never terminated after cross-instance rejection")
}
