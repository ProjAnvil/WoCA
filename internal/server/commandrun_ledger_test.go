package server

// Command-run ledger parity (F3 item 1): /undo and /redo command runs are
// registered, streaming runs — when the run ledger is configured they must
// carry a ledger row and mirror their events like any model run, so a client
// following the stream from another instance (or after a reconnect) replays
// the command turn's events instead of a 404, and the cross-instance
// one_active_run_per_session constraint orders the local command writer
// against runs executing elsewhere.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
)

func TestCommandRunMirrorsLedgerEvents(t *testing.T) {
	a := newScriptedAgent(t, "unused") // the command run never calls the model
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	runID, _, status := postCommandRun(t, srv.URL, "sess-cmd-ledger", "/undo")
	if status != http.StatusOK {
		t.Fatalf("/undo status = %d, want 200", status)
	}

	// The row must reach a terminal status with the thread id recorded,
	// exactly like a locally executed model run.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		row, ok, err := ledger.Row(t.Context(), runID)
		if err == nil && ok && row.Status == "done" && row.ThreadID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, ok, err := ledger.Row(t.Context(), runID)
	if err != nil || !ok || row.Status != "done" || row.ThreadID == "" {
		t.Fatalf("command run row = %+v ok=%v err=%v, want terminal done with a thread id", row, ok, err)
	}

	events, err := ledger.Replay(t.Context(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("mirrored seq = %d at position %d", ev.Seq, i)
		}
		kinds[ev.Kind] = true
	}
	for _, want := range []string{"run_started", "delta", "done"} {
		if !kinds[want] {
			t.Fatalf("missing %q in mirrored command-run kinds %v", want, kinds)
		}
	}
}

// The multi-instance serving shape: the command run executes on server A
// (locally — commands never dispatch), and a client attached to server B
// (same database, no in-memory run) follows the stream from the ledger, the
// same acceptance shape TestCrossInstanceSSEResume pins for model runs.
func TestCommandRunSSEFromAnotherInstance(t *testing.T) {
	a := newScriptedAgent(t, "unused")
	ledger := openRunLedger(t)
	serverA := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	serverB := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srvA := httptest.NewServer(serverA.Handler())
	defer srvA.Close()
	srvB := httptest.NewServer(serverB.Handler())
	defer srvB.Close()

	runID, _, status := postCommandRun(t, srvA.URL, "sess-cmd-cross", "/undo")
	if status != http.StatusOK {
		t.Fatalf("/undo status = %d, want 200", status)
	}

	frames := followSSE(t, srvB.URL+"/api/runs/"+runID+"/events", "")
	if len(frames) == 0 {
		t.Fatal("no frames from B")
	}
	last := frames[len(frames)-1]
	if last.event != "done" {
		t.Fatalf("terminal frame from B = %q, want done", last.event)
	}
	sawDelta := false
	for _, f := range frames {
		if f.event == "delta" {
			sawDelta = true
		}
	}
	if !sawDelta {
		t.Fatalf("frames from B carry no delta: %+v", frames)
	}
}
