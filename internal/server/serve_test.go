package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/slashcmds"
	"github.com/projanvil/woca/internal/tools"
)

// errorLimiter makes every model call fail, so runs end in the runner's error
// path instead of a normal completion.
type errorLimiter struct{ err error }

func (l errorLimiter) Acquire(context.Context, bool) (bool, error) { return false, l.err }

func newFailingAgent(t *testing.T) *agent.Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithRateLimiter(errorLimiter{err: errors.New("model offline")}),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// streamRunEvents subscribes to a run's SSE stream and returns the terminal
// event (type + data), failing on stream end before one arrives.
func streamTerminalEvent(t *testing.T, base, runID string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			t.Fatal("stream ended before a terminal event")
		}
		if ev.Type == "done" || ev.Type == "error" {
			return ev.Type, ev.Data
		}
	}
}

// waitForRunInterrupt subscribes and returns once the run parks at an
// interrupt. The response and its scanner stay with the caller: the SAME
// scanner must keep reading the stream (a fresh scanner would miss events the
// first one buffered ahead).
func waitForRunInterrupt(t *testing.T, base, runID string) (*http.Response, *bufio.Scanner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // the request outlives this helper
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			resp.Body.Close()
			t.Fatal("stream ended before interrupt")
		}
		if ev.Type == "interrupt" {
			return resp, sc
		}
		if ev.Type == "done" || ev.Type == "error" {
			resp.Body.Close()
			t.Fatalf("run ended before parking: %s %s", ev.Type, ev.Data)
		}
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, "127.0.0.1:0") }()

	// Give the listener a moment, then cancel: Serve must return nil.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}

func TestServeListenError(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	// An invalid port fails ListenAndServe immediately (no network egress);
	// Serve must surface the error rather than treating it as shutdown.
	err := s.Serve(context.Background(), "127.0.0.1:-1")
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve on bad addr = %v, want a listen error", err)
	}
}

func TestRunModelErrorEmitsErrorEvent(t *testing.T) {
	s := New(Config{Agent: newFailingAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, _ := createRun(t, ts.URL, "")
	typ, data := streamTerminalEvent(t, ts.URL, runID)
	if typ != "error" || !strings.Contains(data, "model offline") {
		t.Fatalf("terminal event = %s %s, want error mentioning the model failure", typ, data)
	}
}

// A run cancelled while parked at an interrupt must finish with a "done"
// event flagged cancelled — not an error.
func TestRunCancelWhileParked(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, _ := createRun(t, ts.URL, "")
	_, sc := waitForRunInterrupt(t, ts.URL, runID)

	// Cancelling an unknown run 404s; cancelling the live one succeeds.
	if err := postJSON(t, ts.URL+"/api/runs/run-999999/cancel", ""); err == nil {
		t.Fatal("cancel unknown run should 404")
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		if ev.Type == "error" {
			t.Fatalf("cancelled run errored: %s", ev.Data)
		}
		if ev.Type == "done" {
			if !strings.Contains(ev.Data, `"cancelled":true`) {
				t.Fatalf("done payload = %s, want cancelled:true", ev.Data)
			}
			return
		}
	}
}

func TestHandleInterruptParksEachKind(t *testing.T) {
	mkRun := func() (*Run, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		return newRun("ir", nil, ctx, cancel), cancel
	}

	// Unknown interrupt values are rejected outright.
	r0, cancel0 := mkRun()
	defer cancel0()
	if _, err := r0.handleInterrupt(42); err == nil {
		t.Fatal("unknown interrupt value should error")
	}

	cases := []struct {
		value  any
		kind   string
		answer string
	}{
		{agent.PermissionRequest{}, "permission", `{"approve":true}`},
		{agent.AskUserRequest{}, "ask", `{"answers":["A"]}`},
		{agent.PlanApprovalRequest{}, "plan", `{"approve":false}`},
	}
	for _, c := range cases {
		r, cancel := mkRun()
		got := make(chan any, 1)
		go func() {
			v, err := r.handleInterrupt(c.value)
			if err != nil {
				got <- err
			} else {
				got <- v
			}
		}()
		waitPending(t, r)

		// The kind is recorded on the parked state.
		r.mu.Lock()
		kind, payload := r.pending.kind, r.pending.payload
		r.mu.Unlock()
		if kind != c.kind || payload == nil {
			t.Fatalf("pending = %s %v, want kind %q", kind, payload, c.kind)
		}

		// A malformed answer leaves the interrupt parked and returns an error.
		if err := r.answer([]byte("not json")); err == nil {
			t.Fatalf("%s: malformed answer should error", c.kind)
		}
		if err := r.answer([]byte(c.answer)); err != nil {
			t.Fatalf("%s: answer: %v", c.kind, err)
		}
		select {
		case v := <-got:
			if err, ok := v.(error); ok {
				t.Fatalf("%s: handleInterrupt: %v", c.kind, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: interrupt never resumed", c.kind)
		}
		cancel()
	}
}

// waitPending blocks until the run has a parked interrupt.
func waitPending(t *testing.T, r *Run) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		p := r.pending
		r.mu.Unlock()
		if p != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("interrupt never parked")
}

// noFlushWriter is an http.ResponseWriter without Flush, exercising the
// streaming-unsupported branch of the events endpoint.
type noFlushWriter struct {
	header http.Header
	status int
	buf    strings.Builder
}

func (w *noFlushWriter) Header() http.Header         { return w.header }
func (w *noFlushWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *noFlushWriter) WriteHeader(code int)        { w.status = code }

func TestHandleEventsDegradedPaths(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Unknown run id: 404 before any streaming setup.
	resp, err := http.Get(ts.URL + "/api/runs/run-unknown/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run events status = %d, want 404", resp.StatusCode)
	}

	// A ResponseWriter that cannot flush: 500.
	run := newRun("noflush", nil, context.Background(), func() {})
	s.registerRun(run.id, run)
	req := httptest.NewRequest(http.MethodGet, "/api/runs/noflush/events", nil)
	req.SetPathValue("id", run.id)
	w := &noFlushWriter{header: http.Header{}}
	s.handleEvents(w, req)
	if w.status != http.StatusInternalServerError {
		t.Fatalf("non-flusher status = %d, want 500", w.status)
	}

	// An event whose data cannot be marshaled ends the stream silently.
	bad := newRun("baddata", nil, context.Background(), func() {})
	bad.emit(Event{Type: "boom", Data: map[string]any{"ch": make(chan int)}})
	s.registerRun(bad.id, bad)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/runs/baddata/events", nil)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(getResp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if _, ok := readSSEEvent(sc); ok {
		t.Fatal("unmarshalable event should end the stream without emitting it")
	}
	getResp.Body.Close()
}

// A client disconnecting from a live (parked) run must unblock the handler
// through the request-context branch.
func TestHandleEventsClientDisconnect(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, _ := createRun(t, ts.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/runs/"+runID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before interrupt")
		}
		if ev.Type == "interrupt" {
			break
		}
		if ev.Type == "done" || ev.Type == "error" {
			t.Fatalf("run ended early: %s %s", ev.Type, ev.Data)
		}
	}
	// Disconnect: the handler's select must observe ctx.Done.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
			return // connection torn down — handler exited
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stream stayed open after client disconnect")
}

func TestAnswerEndpointErrors(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Unknown run: 404.
	resp, err := http.Post(ts.URL+"/api/runs/run-none/answer", "application/json",
		strings.NewReader(`{"answers":["A"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("answer unknown run status = %d, want 404", resp.StatusCode)
	}

	// A registered-but-not-started run has no pending interrupt: 400.
	run := newRun("idle-run", nil, context.Background(), func() {})
	s.registerRun(run.id, run)
	resp, err = http.Post(ts.URL+"/api/runs/idle-run/answer", "application/json",
		strings.NewReader(`{"answers":["A"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("answer without interrupt status = %d, want 400", resp.StatusCode)
	}

	// A body that fails mid-read is a 400, not a panic.
	failing := httptest.NewRequest(http.MethodPost, "/api/runs/idle-run/answer", errReader{})
	failing.SetPathValue("id", run.id)
	w := httptest.NewRecorder()
	s.handleAnswer(w, failing)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("failing body status = %d, want 400", w.Code)
	}
}

// errReader always fails, to exercise request-body read errors.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("body boom") }

func TestCancelRunWithoutCancelFunc(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	// A run built without a cancel func must not panic on cancel.
	run := newRun("nil-cancel", nil, context.Background(), nil)
	s.registerRun(run.id, run)
	run.cancelRun()

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/runs/nil-cancel/cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel nil-cancel run status = %d, want 200", resp.StatusCode)
	}
}

func TestQueueInputBadJSON(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	runID, _ := createRun(t, ts.URL, "")
	resp, err := http.Post(ts.URL+"/api/runs/"+runID+"/input", "application/json",
		strings.NewReader(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad input JSON status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateRunValidation(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(`{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", code)
	}
	if code := post(`{"prompt":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("empty prompt status = %d, want 400", code)
	}
	// More than maxAttachments attachments.
	atts := make([]string, maxAttachments+1)
	for i := range atts {
		atts[i] = `{"name":"a.png","mime_type":"image/png","data":"YQ=="}`
	}
	body := `{"prompt":"x","attachments":[` + strings.Join(atts, ",") + `]}`
	if code := post(body); code != http.StatusBadRequest {
		t.Fatalf("too many attachments status = %d, want 400", code)
	}
	// Valid MIME but invalid base64.
	if code := post(`{"prompt":"x","attachments":[{"name":"a.png","mime_type":"image/png","data":"!!!"}]}`); code != http.StatusBadRequest {
		t.Fatalf("bad base64 status = %d, want 400", code)
	}
}

// /undo whose restore cannot hit the disk (the target's parent became a
// regular file) surfaces the harness error in the streamed output instead of
// failing the run.
func TestUndoCommandErrorPath(t *testing.T) {
	dir := t.TempDir()
	nested := dir + "/nested"
	target := nested + "/out.txt"
	// Two turns: create the file, then modify it (recording a non-nil
	// "before" state the undo must restore).
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{
					"file_path": target,
					"content":   "v1",
				}},
			}},
			messages.AI("wrote v1"),
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "2", Name: "Write", Args: map[string]any{
					"file_path": target,
					"content":   "v2",
				}},
			}},
			messages.AI("wrote v2"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		WorkDir:        dir,
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop()}).Handler())
	defer ts.Close()

	postRun := func(prompt string) string {
		resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(`{"prompt":"`+prompt+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var created struct {
			RunID string `json:"run_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return driveCommandRun(t, ts.URL, created.RunID)
	}
	postRun("write v1")
	postRun("write v2")
	if data, _ := os.ReadFile(target); string(data) != "v2" {
		t.Fatalf("target = %q, want v2 before undo", data)
	}

	// Replace the parent dir with a regular file: restoring v1 must fail.
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := postRun("/undo")
	if !strings.Contains(out, "Error:") {
		t.Fatalf("/undo output = %q, want an Error: prefix", out)
	}
}

func TestSessionAndStaticHandlerEdges(t *testing.T) {
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("app")}}
	s := New(Config{Agent: newTestAgent(t), Static: static, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Non-GET static requests are 404 (no SPA fallback for mutations).
	resp, err := http.Post(ts.URL+"/some/path", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST static status = %d, want 404", resp.StatusCode)
	}

	// An unknown session id yields an empty transcript, not 404.
	resp, err = http.Get(ts.URL + "/api/sessions/never-seen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	if sess.Messages == nil {
		t.Fatal("messages should serialize as [], not null")
	}

	// fileExists degenerates to true for the root path.
	if !s.fileExists("") {
		t.Fatal("fileExists(\"\") = false, want true")
	}
}

func TestListSessionsDegradedPaths(t *testing.T) {
	// No persistence: the list endpoint returns [] rather than null.
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var list []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if list == nil {
		t.Fatal("session list should be [], not null")
	}

	// A session dir that is a regular file no longer breaks listing: list()
	// reads only the in-memory index (zero filesystem access at request
	// time), so it degrades to an empty 200 list.
	file := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: file})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	resp2, err := http.Get(ts2.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var empty []json.RawMessage
	if err := json.NewDecoder(resp2.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || empty == nil {
		t.Fatalf("unreadable session dir: status = %d list = %v, want 200 []", resp2.StatusCode, empty)
	}
}

func TestWorkspaceTreeEmptyWithoutWorkDir(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("workspace tree = %s, want null", raw)
	}
}

func TestCompletionsBuiltinShadowsSkill(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	// A skill named like a builtin command must not duplicate the entry.
	s.agent.SetSkills([]skills.Skill{{Name: "goal", Description: "shadowed"}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	count, found := 0, false
	for _, e := range got.Skills {
		if e.Name == "goal" {
			count++
			found = true
		}
	}
	if !found || count != 1 {
		t.Fatalf("builtin %q appears %d times (found=%v), want exactly the builtin entry", "goal", count, found)
	}
	if len(got.Skills) != len(slashcmds.Builtin()) {
		t.Fatalf("skills count = %d, want %d (shadowed skill skipped)", len(got.Skills), len(slashcmds.Builtin()))
	}
}

func TestPreviewForLogTruncates(t *testing.T) {
	long := strings.Repeat("字", 250)
	got := previewForLog(long)
	if want := strings.Repeat("字", 200) + "…"; got != want {
		t.Fatalf("previewForLog len = %d, want %d runes + ellipsis", len([]rune(got)), 201)
	}
	if got := previewForLog("short"); got != "short" {
		t.Fatalf("previewForLog(short) = %q", got)
	}
}

func TestAttachmentSizeLimit(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Valid base64 but more than maxAttachmentBytes decoded bytes across the
	// attachments: rejected before the run starts.
	data := base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1024))
	body := fmt.Sprintf(`{"prompt":"x","attachments":[{"name":"big.png","mime_type":"image/png","data":%q}]}`, data)
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized attachments status = %d, want 400", resp.StatusCode)
	}
}

// failFlushWriter flushes fine but fails every write, exercising the SSE
// write-error branch.
type failFlushWriter struct{ header http.Header }

func (w *failFlushWriter) Header() http.Header       { return w.header }
func (w *failFlushWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }
func (w *failFlushWriter) WriteHeader(int)           {}
func (w *failFlushWriter) Flush()                    {}

func TestHandleEventsWriteError(t *testing.T) {
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	run := newRun("write-fail", nil, context.Background(), func() {})
	run.emit(Event{Type: "delta", Data: map[string]any{"text": "hi"}})
	s.registerRun(run.id, run)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/write-fail/events", nil)
	req.SetPathValue("id", run.id)
	w := &failFlushWriter{header: http.Header{}}
	s.handleEvents(w, req) // must return without panicking on the failed write
}

func TestWorkspaceFileTooLargeEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "huge.txt"), make([]byte, workspaceMaxFilePreview+1), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workspace/file?path=huge.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized preview status = %d, want 413", resp.StatusCode)
	}
}
