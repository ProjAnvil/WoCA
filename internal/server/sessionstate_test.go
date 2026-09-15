package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/wocadb"
)

// pollRunStatus waits until GET /api/sessions reports want for the session
// ("" = no run recorded). Only use it for stable states (awaiting, idle);
// transient ones (running before an interrupt) race the run loop.
func pollRunStatus(t *testing.T, ts *httptest.Server, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := "<none>"
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/sessions")
		if err != nil {
			t.Fatal(err)
		}
		var list []sessionSummary
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == sessionID {
				last = s.RunStatus
				if s.RunStatus == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run_status = %q, want %q (timeout)", last, want)
}

// globalEvent is one parsed /api/events frame.
type globalEvent struct {
	Type      string `json:"-"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	RunID     string `json:"run_id"`
}

// collectGlobalEvents streams /api/events into a channel until ctx cancels.
func collectGlobalEvents(ctx context.Context, t *testing.T, ts *httptest.Server) <-chan globalEvent {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := make(chan globalEvent, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev globalEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = globalEvent{Type: strings.TrimSpace(strings.TrimPrefix(line, "event:"))}
			case strings.HasPrefix(line, "data:"):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev)
			case line == "" && ev.Type != "":
				out <- ev
				ev = globalEvent{}
			}
		}
	}()
	return out
}

// expectGlobal drains the channel until a session_state event for sid with
// the wanted status shows up (or times out). Events are consumed in order,
// so consecutive calls assert the transition sequence.
func expectGlobal(t *testing.T, events <-chan globalEvent, sid, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "session_state" && ev.SessionID == sid && ev.Status == want {
				return
			}
		case <-deadline:
			t.Fatalf("no session_state %q event for %s", want, sid)
		}
	}
}

// createSessionViaAPI POSTs /api/sessions and returns its id — the web UI
// always materializes a session before running, which also puts it in the
// listing index immediately.
func createSessionViaAPI(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.SessionID == "" {
		t.Fatalf("create session: %v (%+v)", err, created)
	}
	return created.SessionID
}

// TestSessionRunStateLifecycle drives one full run — start, interrupt park,
// answer, terminal — and verifies the durable state (via the session list)
// and the global event stream at every transition.
func TestSessionRunStateLifecycle(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := collectGlobalEvents(ctx, t, ts)

	runID, sid := createRun(t, ts.URL, createSessionViaAPI(t, ts))
	// Transitions are asserted on the ordered event stream; the stable park
	// state is additionally verified through the decorated session list.
	expectGlobal(t, events, sid, "running")
	pollRunStatus(t, ts, sid, "awaiting")
	expectGlobal(t, events, sid, "awaiting")

	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", `{"answers":["A"]}`); err != nil {
		t.Fatal(err)
	}
	expectGlobal(t, events, sid, "running")

	waitRunEvent(t, ts.URL, runID, "done")
	expectGlobal(t, events, sid, "")
	pollRunStatus(t, ts, sid, "")
}

// waitRunEvent subscribes to the run's stream (with replay) and blocks until
// the wanted event type shows up.
func waitRunEvent(t *testing.T, base, runID, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatalf("stream ended before %s", want)
		}
		if ev.Type == want {
			return
		}
		if ev.Type == "error" {
			t.Fatalf("run error before %s: %s", want, ev.Data)
		}
	}
}

// TestInterruptResolvedInReplay guards the refresh path: a client replaying
// the stream must see interrupt_resolved after the answered interrupt, so a
// rebuilt UI does not resurrect a satisfied interrupt card.
func TestInterruptResolvedInReplay(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, sid := createRun(t, ts.URL, createSessionViaAPI(t, ts))
	waitRunEvent(t, ts.URL, runID, "interrupt")
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", `{"answers":["A"]}`); err != nil {
		t.Fatal(err)
	}
	waitRunEvent(t, ts.URL, runID, "done")
	pollRunStatus(t, ts, sid, "")

	// A fresh subscription replays the full history.
	resp, err := http.Get(ts.URL + "/api/runs/" + runID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	sawInterrupt, sawResolvedAfter := false, false
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			break
		}
		switch ev.Type {
		case "interrupt":
			sawInterrupt = true
		case "interrupt_resolved":
			if sawInterrupt {
				sawResolvedAfter = true
			}
		}
	}
	if !sawInterrupt || !sawResolvedAfter {
		t.Fatalf("replay: interrupt=%v resolved-after=%v", sawInterrupt, sawResolvedAfter)
	}
}

// TestGlobalEventsSeedsSnapshot verifies a fresh /api/events connection first
// receives the current state, so connecting is itself a resync.
func TestGlobalEventsSeedsSnapshot(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	s.markSessionState("sess-x", wocadb.SessionAwaiting, "run-x")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := collectGlobalEvents(ctx, t, ts)
	expectGlobal(t, events, "sess-x", "awaiting")
}

// TestReconcileStaleSessionState clears rows whose owning process died (a
// crash between mark and clear) and keeps rows owned by live or remote
// processes.
func TestReconcileStaleSessionState(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	host := ownerHost(newStateOwner())
	seed := []wocadb.SessionState{
		{SessionID: "dead", Status: wocadb.SessionRunning, RunID: "r1", Owner: "99999999@" + host, UpdatedAt: 1},
		{SessionID: "self", Status: wocadb.SessionRunning, RunID: "r2", Owner: newStateOwner(), UpdatedAt: 2},
		{SessionID: "remote", Status: wocadb.SessionRunning, RunID: "r3", Owner: "1@elsewhere", UpdatedAt: 3},
	}
	for _, st := range seed {
		if err := db.SetSessionState(st); err != nil {
			t.Fatal(err)
		}
	}

	New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), RunStateDB: db})
	rows, err := db.SessionStates()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SessionID] = true
	}
	if got["dead"] {
		t.Fatal("stale row (dead pid) not reconciled")
	}
	if !got["self"] || !got["remote"] {
		t.Fatalf("live/remote rows must survive reconcile, got %v", got)
	}
}

// TestDeleteSessionForgetsState: deleting a session drops its run-state row.
func TestDeleteSessionForgetsState(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	s.markSessionState("sess-doomed", wocadb.SessionRunning, "run-doomed")
	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/sessions/sess-doomed", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	if _, ok := s.state.Snapshot()["sess-doomed"]; ok {
		t.Fatal("state row survived session delete")
	}
}
