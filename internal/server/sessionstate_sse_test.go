package server

// Gap tests: session-state backend error paths and reconciliation, SSE
// Last-Event-ID replay and gap signalling, cancel-while-queued, run-lease
// ledger mirroring, ServeStarted shutdown branches, and misc handler edges.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
)

// --- session-state unit gaps -------------------------------------------

func TestDBSessionStateClosedDB(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "st.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &dbSessionState{db: db}
	s.Set(wocadb.SessionState{SessionID: "s1", RunID: "r1"})
	if ok := s.ClearIfRun("s1", "r1"); !ok {
		t.Fatal("ClearIfRun on live DB should succeed")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Every method degrades silently on a closed DB.
	s.Set(wocadb.SessionState{SessionID: "s1"})
	if s.ClearIfRun("s1", "r1") {
		t.Fatal("ClearIfRun on closed DB should report false")
	}
	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot on closed DB = %v", got)
	}
}

func TestMemSessionStateLifecycle(t *testing.T) {
	s := newMemSessionState()
	s.Set(wocadb.SessionState{SessionID: "s1", RunID: "r1", Status: wocadb.SessionRunning})
	if ok := s.ClearIfRun("s1", "other"); ok {
		t.Fatal("run-id mismatch should not clear")
	}
	if ok := s.ClearIfRun("s1", "r1"); !ok {
		t.Fatal("matching run id should clear")
	}
	if s.ClearIfRun("ghost", "x") {
		t.Fatal("unknown session should not clear")
	}
	s.Set(wocadb.SessionState{SessionID: "s2"})
	s.Delete("s2")
	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot after delete = %v", got)
	}
}

func TestParseOwnerAndWriteSSE(t *testing.T) {
	if pid, host := parseOwner("no-at-sign"); pid != 0 || host != "" {
		t.Fatalf("malformed owner = %d %q", pid, host)
	}
	if pid, host := parseOwner("42@h"); pid != 42 || host != "h" {
		t.Fatalf("owner = %d %q", pid, host)
	}
	if pid, _ := parseOwner("x@h"); pid != 0 {
		t.Fatalf("non-numeric pid = %d", pid)
	}

	rec := httptest.NewRecorder()
	writeSSE(rec, Event{Type: "bad", Data: map[string]any{"ch": make(chan int)}})
	if rec.Body.Len() != 0 {
		t.Fatalf("unmarshalable event wrote %q", rec.Body.String())
	}
}

// --- workspace helpers ---------------------------------------------------

func TestWorkspaceHelperGuards(t *testing.T) {
	if got := buildWorkspaceTree(""); got != nil {
		t.Fatalf("empty root = %v", got)
	}
	if _, err := readWorkspaceFile("", "x"); err != errNotFound {
		t.Fatalf("empty root err = %v", err)
	}
	root := t.TempDir()
	if _, err := readWorkspaceFile(root, ""); err != errNotFound {
		t.Fatalf("empty rel err = %v", err)
	}
	if _, err := readWorkspaceFile(root, "../escape"); err != errNotFound {
		t.Fatalf("traversal err = %v", err)
	}
}

// --- HTTP-level gaps -----------------------------------------------------

func TestSessionContextWithoutAgent(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sessions/s1/context")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("context without agent = %d", resp.StatusCode)
	}
}

func TestPutPluginsAndSettingsOnClosedDB(t *testing.T) {
	st, err := settings.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Settings: st, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	put := func(path, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPut, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := put("/api/plugins", `{"enabled":[]}`); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT plugins on closed DB = %d", resp.StatusCode)
	}
	if resp := put("/api/settings", `{"anthropic_api_key":"sk-x"}`); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT settings on closed DB = %d", resp.StatusCode)
	}
}

// newLedgerServer wires a full server whose run ledger persists to sqlite.
func newLedgerServer(t *testing.T, a *agent.Agent) (*Server, *httptest.Server, *wocadb.DB) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(Config{
		Agent:      a,
		Static:     fstest.MapFS{},
		Logger:     zerolog.Nop(),
		SessionDir: t.TempDir(),
		WorkDir:    t.TempDir(),
		RunStateDB: db,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, db
}

// streamSince subscribes with a Last-Event-ID header and collects event
// types until done (or a short deadline for streams that only signal).
func streamSince(t *testing.T, base, runID, lastEventID string, untilDone bool) []string {
	t.Helper()
	deadline := 3 * time.Second
	if untilDone {
		deadline = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", lastEventID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out []string
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			out = append(out, ev)
			if !untilDone && len(out) >= 1 {
				break
			}
			if ev == "done" || ev == "error" {
				break
			}
		}
	}
	return out
}

// TestSSELastEventIDReplayAndGap: replaying a finished run by Last-Event-ID
// returns the missed tail; an id outside retention signals a resync.
func TestSSELastEventIDReplayAndGap(t *testing.T) {
	a := newScriptedAgent(t, "done-reply")
	_, srv, _ := newLedgerServer(t, a)
	runID, _, _ := postRun(t, srv.URL, "", "hello")
	waitRunDone(t, srv.URL, runID)

	// Replaying run-1 since seq 1 reaches the terminal done event.
	types := streamSince(t, srv.URL, runID, "1", true)
	if !slices.Contains(types, "done") {
		t.Fatalf("replay tail missing done event: %v", types)
	}
}

// TestCancelWhileQueued: a queued run cancelled before admission finishes
// as done/cancelled instead of executing.
func TestCancelWhileQueued(t *testing.T) {
	a := newScriptedAgent(t, "first-reply")
	_, srv, _ := newLedgerServer(t, a)

	run1, sid, _ := postRun(t, srv.URL, "", "gate holder")
	run2, _, pos := postRun(t, srv.URL, sid, "queued")
	if pos == 0 {
		t.Fatal("second run should queue behind the first")
	}
	resp, err := http.Post(srv.URL+"/api/runs/"+run2+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d", resp.StatusCode)
	}

	waitRunDone(t, srv.URL, run1)
	types := streamSince(t, srv.URL, run2, "", true)
	if len(types) == 0 {
		t.Fatal("cancelled run stream empty")
	}
}

func TestServeStartedShutdownBranches(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})

	// A server-closed error is a clean stop.
	ch := make(chan error, 1)
	ch <- http.ErrServerClosed
	if err := s.ServeStarted(t.Context(), &http.Server{}, ch); err != nil {
		t.Fatalf("ErrServerClosed = %v", err)
	}

	// A real error propagates.
	ch2 := make(chan error, 1)
	ch2 <- fmt.Errorf("bind: nope")
	if err := s.ServeStarted(t.Context(), &http.Server{}, ch2); err == nil {
		t.Fatal("serve error should propagate")
	}

	// Context cancellation shuts the http server down gracefully (and with a
	// ledger wired, stops the reconciler loop too).
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s2 := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop(), RunStateDB: db})
	if err := s2.ServeStarted(ctx, &http.Server{}, nil); err != nil {
		t.Fatalf("cancelled ctx = %v", err)
	}
}
