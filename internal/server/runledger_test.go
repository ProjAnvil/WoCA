package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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
		row, ok, err := ledger.Row(t.Context(), out.RunID)
		if err == nil && ok && row.Status == wantStatus && (wantStatus != "done" || row.ThreadID != "") {
			return out.RunID
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, ok, _ := ledger.Row(t.Context(), out.RunID)
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

	row, ok, err := ledger.Row(t.Context(), runID)
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
	if err := ledger.Enqueue(t.Context(), runstate.Run{
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

// sseReader parses the raw SSE stream into (id, event, data) frames.
type sseFrame struct {
	id    string
	event string
	data  string
}

func readSSE(t *testing.T, resp *http.Response, stop <-chan struct{}) []sseFrame {
	t.Helper()
	frames := make(chan sseFrame, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case line == "":
				if cur.event != "" {
					frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
		close(frames)
	}()
	var out []sseFrame
	deadline := time.After(15 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			out = append(out, f)
			if f.event == "done" || f.event == "error" || f.event == "resync" {
				return out
			}
		case <-stop:
			return out
		case <-deadline:
			t.Fatal("SSE stream never terminated")
			return out
		}
	}
}

func TestRunEventsMirrorToLedger(t *testing.T) {
	a := newScriptedAgent(t, "mirror-reply")
	ledger := openRunLedger(t)
	s := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	runID := startAndAwaitRun(t, s, srv, ledger, "sess-mirror", "mirror me", "done")

	events, err := ledger.Replay(t.Context(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no mirrored events")
	}
	kinds := map[string]bool{}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("mirrored seq = %d at position %d", ev.Seq, i+1)
		}
		kinds[ev.Kind] = true
	}
	for _, want := range []string{"run_started", "delta", "done"} {
		if !kinds[want] {
			t.Fatalf("missing %q in mirrored kinds %v", want, kinds)
		}
	}
}

// TestCrossInstanceSSEResume proves the Phase 2 acceptance shape: the run
// executes on server A while a client attached to server B (same database,
// no in-memory run) follows the stream live, and Last-Event-ID resume from
// B continues exactly after the cursor.
func TestCrossInstanceSSEResume(t *testing.T) {
	a := newScriptedAgent(t, "cross-reply")
	ledger := openRunLedger(t)
	dir := t.TempDir()
	serverA := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: dir, WorkDir: t.TempDir(), RunLedger: ledger,
	})
	serverB := New(Config{
		Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: dir, WorkDir: t.TempDir(), RunLedger: ledger,
	})
	srvA := httptest.NewServer(serverA.Handler())
	defer srvA.Close()
	srvB := httptest.NewServer(serverB.Handler())
	defer srvB.Close()

	// Start the run on A.
	body, _ := json.Marshal(map[string]any{"session_id": "sess-cross", "prompt": "cross instance"})
	resp, err := http.Post(srvA.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.RunID == "" {
		t.Fatal("no run id from A")
	}

	// Follow from B until the terminal event.
	frames := followSSE(t, srvB.URL+"/api/runs/"+out.RunID+"/events", "")
	if len(frames) == 0 {
		t.Fatal("no frames from B")
	}
	last := frames[len(frames)-1]
	if last.event != "done" {
		t.Fatalf("terminal frame from B = %q", last.event)
	}
	// Resume from the mid-stream cursor: only the tail after it arrives.
	half := frames[len(frames)/2]
	tail := followSSE(t, srvB.URL+"/api/runs/"+out.RunID+"/events", half.id)
	if len(tail) == 0 {
		t.Fatal("resume produced no frames")
	}
	firstSeq, _ := strconv.ParseInt(tail[0].id, 10, 64)
	cursor, _ := strconv.ParseInt(half.id, 10, 64)
	if firstSeq != cursor+1 {
		t.Fatalf("resume starts at seq %d, want %d", firstSeq, cursor+1)
	}
}

// followSSE opens an SSE GET (with an optional Last-Event-ID) and returns
// the frames up to and including the terminal one.
func followSSE(t *testing.T, url, lastEventID string) []sseFrame {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	stop := make(chan struct{})
	timer := time.AfterFunc(12*time.Second, func() { close(stop) })
	defer timer.Stop()
	return readSSE(t, resp, stop)
}

// dbSessionState degrades instead of failing on store errors, and snapshot
// paths handle both live and nil stores.
func TestSessionStateDegradesOnStoreErrors(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "ss.db"))
	if err != nil {
		t.Fatal(err)
	}
	st := &dbSessionState{db: db}
	ctx := t.Context()
	_ = ctx
	st.Set(wocadb.SessionState{SessionID: "s1", Status: "running", RunID: "r1"})
	if ok := st.ClearIfRun("s1", "r1"); !ok {
		t.Fatal("ClearIfRun after Set failed")
	}
	st.Set(wocadb.SessionState{SessionID: "s2", Status: "running", RunID: "r2"})
	st.Delete("s2")
	if st.ClearIfRun("s2", "r2") {
		t.Fatal("cleared a deleted session")
	}
	if snap := st.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot after deletes = %v", snap)
	}
	// A closed store degrades on every path without panicking.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st.Set(wocadb.SessionState{SessionID: "s3"}) // error swallowed
	_ = st.ClearIfRun("s3", "r")                 // false
	st.Delete("s3")                              // error swallowed
	_ = st.Snapshot()                            // empty on error
}

// The in-memory state store covers the same interface contract.
func TestMemSessionStateContract(t *testing.T) {
	st := newMemSessionState()
	st.Set(wocadb.SessionState{SessionID: "s1", Status: wocadb.SessionRunning, RunID: "r1"})
	snap := st.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("mem snapshot = %v", snap)
	}
	if ok := st.ClearIfRun("s1", "r1"); !ok {
		t.Fatal("mem ClearIfRun failed")
	}
	if st.ClearIfRun("s1", "r9") {
		t.Fatal("mem cleared wrong run")
	}
	if snap := st.Snapshot(); len(snap) != 0 {
		t.Fatalf("mem snapshot after clear = %v", snap)
	}
}
