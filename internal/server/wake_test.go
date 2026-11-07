package server

// Idle wake driver integration tests (Task 8): a background notification
// queued for an idle session starts a wake run that delivers it. The five
// scenarios mirror the design's review focus — plain delivery, concurrent
// coalescing, the kill switch, the chain budget with reset, and session
// affinity. Every wait is deadline-bounded; the scripted model cycles, so
// each scenario pins the response sequence that keeps its run chain bounded.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// wakeToolSeq mints unique tool-call ids across runs: the scripted model
// repeats its messages verbatim, and a replayed id inside one checkpoint
// thread would alias the tool round's routing.
var wakeToolSeq atomic.Int64

// bgShellCall scripts one AI turn that starts one background shell per
// command — a single tool round arms them all, so their completions land
// close together (the coalescing scenario).
func bgShellCall(commands ...string) messages.Message {
	calls := make([]messages.ToolCall, len(commands))
	for i, cmd := range commands {
		calls[i] = messages.ToolCall{
			ID:   fmt.Sprintf("bg-%d", wakeToolSeq.Add(1)),
			Name: "Bash",
			Args: map[string]any{"command": cmd, "run_in_background": true},
		}
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: calls}
}

// newWakeAgent builds an agent whose model cycles the scripted responses.
// perCallDelay (via the FakeChatModel rate limiter) keeps a run in flight
// across a shell completion when a scenario needs the busy-session path;
// ModeBypass keeps Bash from parking on a permission interrupt.
func newWakeAgent(t *testing.T, perCallDelay time.Duration, responses ...messages.Message) *agent.Agent {
	t.Helper()
	opts := []language.FakeChatModelOption{
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(responses...),
	}
	if perCallDelay > 0 {
		opts = append(opts, language.WithRateLimiter(delayLimiter{d: perCallDelay}))
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(opts...),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// newWakeServer serves a wake-test server; st may be nil (policy defaults).
func newWakeServer(t *testing.T, a *agent.Agent, st *settings.Store) (*Server, *httptest.Server) {
	t.Helper()
	s := New(Config{
		Agent:      a,
		Static:     fstest.MapFS{},
		Logger:     zerolog.Nop(),
		SessionDir: t.TempDir(),
		WorkDir:    t.TempDir(),
		Settings:   st,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// wakeSettings opens a persisted settings store with the auto-wake keys
// applied (kv is key/value pairs).
func wakeSettings(t *testing.T, kv ...string) *settings.Store {
	t.Helper()
	st, err := settings.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := st.Load()
	for i := 0; i+1 < len(kv); i += 2 {
		if err := settings.Apply(&cfg, kv[i], kv[i+1]); err != nil {
			t.Fatalf("apply %s=%s: %v", kv[i], kv[i+1], err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return st
}

// registeredRuns snapshots the run-registration order.
func registeredRuns(t *testing.T, s *Server) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runOrder...)
}

// waitForRuns blocks until at least n runs are registered.
func waitForRuns(t *testing.T, s *Server, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ids := registeredRuns(t, s); len(ids) >= n {
			return ids
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d runs (have %d)", n, len(registeredRuns(t, s)))
	return nil
}

// waitRunDoneInProc polls the registry until the run is terminal (the wake
// runs have no HTTP response to await, so tests watch the registry).
func waitRunDoneInProc(t *testing.T, s *Server, runID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.lookupRun(runID); r != nil && r.isDone() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never finished", runID)
}

// replayRunEvents fetches a finished run's SSE replay up to its terminal
// frame (handleEvents replays history to late subscribers).
func replayRunEvents(t *testing.T, base, runID string) []sseFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	return readSSE(t, resp, nil)
}

// runStartedData decodes the run_started frame's data payload.
func runStartedData(t *testing.T, frames []sseFrame) map[string]any {
	t.Helper()
	for _, f := range frames {
		if f.event != EventRunStarted {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(f.data), &data); err != nil {
			t.Fatalf("bad run_started data %q: %v", f.data, err)
		}
		return data
	}
	t.Fatal("no run_started frame in replay")
	return nil
}

// sessionMessages loads the persisted transcript via the public API.
func sessionMessages(t *testing.T, base, sid string) []messages.Message {
	t.Helper()
	resp, err := http.Get(base + "/api/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session %s: status %d", sid, resp.StatusCode)
	}
	var out struct {
		Messages []messages.Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Messages
}

// waitForTranscript polls the persisted transcript until pred holds.
func waitForTranscript(t *testing.T, base, sid, desc string, pred func([]messages.Message) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pred(sessionMessages(t, base, sid)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in session %s", desc, sid)
}

// waitForQueueCount polls the agent queue until session holds n items.
func waitForQueueCount(t *testing.T, a *agent.Agent, session string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.QueuedInputCountFor(session) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queued count for %s = %d, want %d", session, a.QueuedInputCountFor(session), n)
}

// subscribeGlobalEvents streams /api/events into a channel until stop. The
// subscription is live-only, so it must be open before the event of interest
// fires (wake_capped rides the global stream).
func subscribeGlobalEvents(t *testing.T, base string) (<-chan sseFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ch := make(chan sseFrame, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if cur.event != "" {
					select {
					case ch <- cur:
					case <-ctx.Done():
						return
					}
				}
				cur = sseFrame{}
			}
		}
	}()
	return ch, cancel
}

// awaitEvent waits for the next frame of evType on the stream.
func awaitEvent(t *testing.T, frames <-chan sseFrame, evType string) sseFrame {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("global events stream ended before %s", evType)
			}
			if f.event == evType {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s on /api/events", evType)
		}
	}
}

// taggedBlocks counts the tagged notification blocks across the transcript's
// Human messages (a coalesced wake message carries several in one Human).
func taggedBlocks(msgs []messages.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == messages.RoleHuman {
			n += strings.Count(m.Content, "<background-task-completed>")
		}
	}
	return n
}

// Scenario 1: a shell finishing while the session is idle starts one wake
// run; its run_started is annotated auto/trigger and the notification text
// reaches the persisted transcript.
func TestWakeDeliversIdleShellNotification(t *testing.T) {
	// Cycle: the user run arms a 0.2s shell then answers; the wake run gets
	// a plain reply (no re-arm — the chain stops here by scripting).
	a := newWakeAgent(t, 0,
		bgShellCall("sleep 0.2 && echo bg-done"),
		messages.AI("armed"),
		messages.AI("noted"),
	)
	s, ts := newWakeServer(t, a, nil)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start the shell")
	waitRunDoneInProc(t, s, runID)

	// The shell finishes while idle; the driver starts the second run.
	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	if wakeID == runID {
		t.Fatalf("second registered run is the user run itself (%s)", wakeID)
	}
	waitRunDoneInProc(t, s, wakeID)

	wakeData := runStartedData(t, replayRunEvents(t, ts.URL, wakeID))
	if auto, _ := wakeData["auto"].(bool); !auto {
		t.Fatalf("wake run run_started data = %v, want auto:true", wakeData)
	}
	if trig, _ := wakeData["trigger"].(string); trig != "background-task-completed" {
		t.Fatalf("wake trigger = %q, want background-task-completed", trig)
	}
	// Only wake runs carry the marker.
	userData := runStartedData(t, replayRunEvents(t, ts.URL, runID))
	if _, has := userData["auto"]; has {
		t.Fatalf("user run run_started data = %v, must not carry auto", userData)
	}

	// The coalesced notification reached the transcript.
	waitForTranscript(t, ts.URL, sid, "notification human message", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "<background-task-completed>") &&
				strings.Contains(m.Content, "bg-done") {
				return true
			}
		}
		return false
	})

	// Quiesce: nothing re-armed, so no further runs appear.
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs after settle, want exactly 2", n)
	}
}

// Scenario 2: two shells armed in one tool round finish while the session is
// busy (inside the run's final, rate-limited model call) — the observer must
// not wake mid-run; the clean finish starts exactly ONE wake run whose single
// Human message coalesces both notifications.
func TestWakeCoalescesConcurrentCompletions(t *testing.T) {
	a := newWakeAgent(t, 400*time.Millisecond,
		bgShellCall("sleep 0.1 && echo bg-one", "sleep 0.1 && echo bg-two"),
		messages.AI("armed"),
		messages.AI("noted"),
	)
	s, ts := newWakeServer(t, a, nil)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start both shells")
	waitRunDoneInProc(t, s, runID) // ~800ms: shells finish inside call 2

	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	waitRunDoneInProc(t, s, wakeID)

	time.Sleep(300 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs total, want exactly 2 (one user + one wake)", n)
	}
	wakeData := runStartedData(t, replayRunEvents(t, ts.URL, wakeID))
	if auto, _ := wakeData["auto"].(bool); !auto {
		t.Fatalf("wake run run_started data = %v, want auto:true", wakeData)
	}

	// One coalesced Human message carrying both tagged blocks.
	var tagged []messages.Message
	for _, m := range sessionMessages(t, ts.URL, sid) {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "<background-task-completed>") {
			tagged = append(tagged, m)
		}
	}
	if len(tagged) != 1 {
		t.Fatalf("%d notification Human messages, want exactly 1 (coalesced): %+v", len(tagged), tagged)
	}
	if got := strings.Count(tagged[0].Content, "<background-task-completed>"); got != 2 {
		t.Fatalf("coalesced message carries %d tagged blocks, want 2: %q", got, tagged[0].Content)
	}
	if !strings.Contains(tagged[0].Content, "bg-one") || !strings.Contains(tagged[0].Content, "bg-two") {
		t.Fatalf("coalesced message missing a shell payload: %q", tagged[0].Content)
	}
}

// Scenario 3: auto-wake=off — the notification queues but nothing wakes;
// the item is never drained.
func TestWakeDisabledBySettings(t *testing.T) {
	st := wakeSettings(t, settings.KeyAutoWake, "false")
	a := newWakeAgent(t, 0,
		bgShellCall("sleep 0.2 && echo bg-done"),
		messages.AI("armed"),
		messages.AI("user reply"),
	)
	s, ts := newWakeServer(t, a, st)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start the shell")
	waitRunDoneInProc(t, s, runID)

	waitForQueueCount(t, a, "s1", 1) // the shell finished and queued
	time.Sleep(300 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 1 {
		t.Fatalf("%d runs with auto-wake off, want 1", n)
	}
	if n := a.QueuedInputCountFor("s1"); n != 1 {
		t.Fatalf("queued count = %d, want 1 (nothing drained)", n)
	}
	for _, m := range sessionMessages(t, ts.URL, sid) {
		if strings.Contains(m.Content, "<background-task-completed>") {
			t.Fatalf("notification delivered with auto-wake off: %q", m.Content)
		}
	}
}

// Scenario 4: the chain budget. Every run re-arms a shell, so with
// auto-wake-limit=2 exactly two wake runs follow the user run; the third
// completion is capped (wake_capped on the global stream, item stays
// queued). A user POST resets the budget, delivers the queued item, and
// earns two more wake runs before the second cap.
func TestWakeChainCappedAndUserRunResets(t *testing.T) {
	st := wakeSettings(t, settings.KeyAutoWakeLimit, "2")
	a := newWakeAgent(t, 0,
		bgShellCall("sleep 0.2 && echo bg-done"),
		messages.AI("armed"),
	)
	s, ts := newWakeServer(t, a, st)
	frames, stop := subscribeGlobalEvents(t, ts.URL)
	defer stop()

	runID, sid, _ := postRun(t, ts.URL, "s1", "go")
	waitRunDoneInProc(t, s, runID)

	// Wakes 1 and 2 run within the budget.
	ids := waitForRuns(t, s, 3)
	for _, id := range ids[1:] {
		waitRunDoneInProc(t, s, id)
		if d := runStartedData(t, replayRunEvents(t, ts.URL, id)); func() bool {
			b, _ := d["auto"].(bool)
			return !b
		}() {
			t.Fatalf("run %s run_started = %v, want auto:true", id, d)
		}
	}

	// The third completion hits the cap: the event fires, the item stays
	// queued, and no fourth run appears.
	capped := awaitEvent(t, frames, EventWakeCapped)
	var capData struct {
		Session string `json:"session"`
		Queued  int    `json:"queued"`
	}
	if err := json.Unmarshal([]byte(capped.data), &capData); err != nil {
		t.Fatalf("bad wake_capped data %q: %v", capped.data, err)
	}
	if capData.Session != sid || capData.Queued != 1 {
		t.Fatalf("wake_capped = %+v, want session %s queued 1", capData, sid)
	}
	waitForQueueCount(t, a, sid, 1)
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 3 {
		t.Fatalf("%d runs after cap, want 3", n)
	}

	// The user run resets the budget and delivers the queued item...
	userRun, _, _ := postRun(t, ts.URL, sid, "your turn")
	waitRunDoneInProc(t, s, userRun)
	if d := runStartedData(t, replayRunEvents(t, ts.URL, userRun)); func() bool {
		_, has := d["auto"]
		return has
	}() {
		t.Fatalf("user run marked auto: %v", d)
	}
	// ...so the shell it armed earns two more wakes before the second cap.
	ids = waitForRuns(t, s, 6)
	for _, id := range ids[4:] {
		waitRunDoneInProc(t, s, id)
	}
	second := awaitEvent(t, frames, EventWakeCapped)
	if !strings.Contains(second.data, `"session":"`+sid+`"`) {
		t.Fatalf("second wake_capped = %q, want session %s", second.data, sid)
	}

	// Final state: six runs, five delivered notification blocks (two wakes
	// + the prepended capped item + two post-reset wakes), one item queued.
	settle := 250 * time.Millisecond
	time.Sleep(settle)
	if n := len(registeredRuns(t, s)); n != 6 {
		t.Fatalf("%d runs after second cap, want 6", n)
	}
	waitForQueueCount(t, a, sid, 1)
	waitForTranscript(t, ts.URL, sid, "five delivered notification blocks", func(msgs []messages.Message) bool {
		return taggedBlocks(msgs) == 5
	})
}

// Scenario 5: session affinity — a notification scoped to s2 never rides
// along with s1's runs, whatever run drains s1's queue.
func TestWakeSessionAffinity(t *testing.T) {
	st := wakeSettings(t, settings.KeyAutoWake, "false") // isolate drain affinity
	a := newWakeAgent(t, 0,
		bgShellCall("sleep 0.2 && echo bg-one"),
		messages.AI("armed"),
		messages.AI("user reply"),
	)
	s, ts := newWakeServer(t, a, st)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start the shell")
	waitRunDoneInProc(t, s, runID)

	// Queue s2's notification directly through the agent seam — arming it
	// via a real shell would need a second scripted model. It must never
	// enter s1's runs.
	a.QueueNotificationFor("s2", "background-task-completed",
		"Background shell 9 finished (exit code 0): sleep 20 && echo bg-two\nOutput (tail):\nbg-two")

	waitForQueueCount(t, a, "s1", 1)
	waitForQueueCount(t, a, "s2", 1)

	userRun, _, _ := postRun(t, ts.URL, sid, "your turn")
	waitRunDoneInProc(t, s, userRun)
	waitForTranscript(t, ts.URL, sid, "s1 notification delivered, s2's absent", func(msgs []messages.Message) bool {
		tagged, sawOne, sawTwo := 0, false, false
		for _, m := range msgs {
			if m.Role != messages.RoleHuman || !strings.Contains(m.Content, "<background-task-completed>") {
				continue
			}
			tagged++
			sawOne = sawOne || strings.Contains(m.Content, "bg-one")
			sawTwo = sawTwo || strings.Contains(m.Content, "bg-two")
		}
		return tagged == 1 && sawOne && !sawTwo
	})

	if n := a.QueuedInputCountFor("s2"); n != 1 {
		t.Fatalf("s2 queued = %d, want 1 (never drained by s1's runs)", n)
	}
	for _, m := range sessionMessages(t, ts.URL, sid) {
		if strings.Contains(m.Content, "bg-two") {
			t.Fatalf("s2 notification leaked into s1's transcript: %q", m.Content)
		}
	}
	// s2 never ran at all: no transcript of its own.
	if msgs := sessionMessages(t, ts.URL, "s2"); len(msgs) != 0 {
		t.Fatalf("s2 has a transcript: %+v", msgs)
	}
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs, want the 2 user runs only", n)
	}
}

// Scenario 6: workspace affinity — a session whose recorded workspace
// differs from the server workdir has its wake run execute in the session's
// workspace, exactly like the session's user runs. The wake driver has no
// request to carry a workspace, so it must resolve it from the session
// store; the memory engine's per-run retain (which keys off the run ctx's
// workspace, cf. TestRunScopesMemoryToSessionWorkspace) makes the run's
// workspace observable.
func TestWakeRunUsesSessionWorkspace(t *testing.T) {
	f := &fakeMemEngine{}
	a := newWakeAgent(t, 0,
		bgShellCall("sleep 0.2 && echo bg-done"),
		messages.AI("armed"),
		messages.AI("noted"),
	)
	a.SetMemoryEngine(f)
	s, ts := newWakeServer(t, a, nil) // WorkDir = one temp dir...

	// ...and the session is created against a DIFFERENT workspace, the way
	// the web client creates sessions (activeWorkspace.path).
	ws := t.TempDir()
	resp, err := http.Post(ts.URL+"/api/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"workspace":%q}`, ws)))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.SessionID == "" {
		t.Fatal("no session id from POST /api/sessions")
	}
	sid := created.SessionID

	// The user run repeats the workspace per run (again mirroring the web
	// client); its completion persists the session meta under that workspace.
	body := fmt.Sprintf(`{"prompt":"start the shell","session_id":%q,"workspace":%q}`, sid, ws)
	resp, err = http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var runCreated struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&runCreated); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	runID := runCreated.RunID
	waitRunDoneInProc(t, s, runID)

	// The shell finishes while idle; the driver starts the wake run.
	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	waitRunDoneInProc(t, s, wakeID)

	// Quiesce: the wake reply arms nothing, so no further runs appear.
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs after settle, want exactly 2", n)
	}

	// Both retains keyed off the session workspace: the user run's (request
	// workspace) and — the point — the wake run's, which only resolves
	// through the session store; before the fix it fell back to the server
	// workdir.
	ev := f.Events()
	if len(ev) != 2 {
		t.Fatalf("memory events = %v, want 2 (user + wake)", ev)
	}
	for i, e := range ev {
		if !strings.HasPrefix(e, ws+"|") {
			t.Fatalf("memory event %d = %q, want workspace %s", i, e, ws)
		}
	}
}

// newWakeAgentReg is newWakeAgent over a caller-held tool registry, so a
// scenario can drive the shell manager directly — KillFor with a StopOrigin
// is the seam the panel/REST user stop calls. perCallDelay (see newWakeAgent)
// holds a run open across a completion when the busy-session path needs it.
func newWakeAgentReg(t *testing.T, perCallDelay time.Duration, reg *tools.Registry, responses ...messages.Message) *agent.Agent {
	t.Helper()
	opts := []language.FakeChatModelOption{
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(responses...),
	}
	if perCallDelay > 0 {
		opts = append(opts, language.WithRateLimiter(delayLimiter{d: perCallDelay}))
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(opts...),
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Scenario D1: a user-initiated stop never wakes the session. The shell is
// killed through the manager with StopByUser (the panel/REST origin); the
// completion notification is attributed ("killed by the user") and queued
// Quiet, so no wake run starts within the settle window, and the next USER
// run delivers it at its start.
func TestUserStoppedShellIsQuietUntilNextUserRun(t *testing.T) {
	// Cycle: the user run arms a long shell and finishes; the follow-up user
	// run receives the quiet notification at its start (no wake turn between).
	reg := tools.NewRegistry(t.TempDir())
	a := newWakeAgentReg(t, 0, reg,
		bgShellCall("echo pre-kill-output && sleep 30"),
		messages.AI("armed"),
		messages.AI("noted"),
	)
	s, ts := newWakeServer(t, a, nil)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start the shell")
	waitRunDoneInProc(t, s, runID)

	bg := reg.Background()
	shellID := ""
	for _, st := range bg.ListFor("s1") {
		if st.Running {
			shellID = st.ID
		}
	}
	if shellID == "" {
		t.Fatal("no running shell after the user run")
	}
	if _, already, err := bg.KillFor("s1", shellID, tools.StopByUser); err != nil || already {
		t.Fatalf("KillFor(user) = err %v already %v", err, already)
	}

	// The completion queued Quiet: present as a pending notification, never
	// wake-worthy.
	waitForQueueCount(t, a, "s1", 1)
	if n := a.WakeableCountFor("s1"); n != 0 {
		t.Fatalf("wakeable count = %d, want 0 (a user stop must not wake)", n)
	}

	// D1: no wake run appears.
	time.Sleep(500 * time.Millisecond)
	if ids := registeredRuns(t, s); len(ids) != 1 {
		t.Fatalf("%d runs after the user stop, want 1 (no wake run)", len(ids))
	}

	// The next USER run delivers the attributed notification at its start.
	nextID, _, _ := postRun(t, ts.URL, "s1", "what happened to my shell")
	waitRunDoneInProc(t, s, nextID)
	waitForTranscript(t, ts.URL, sid, "user-stop notification", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "background-task-completed") &&
				strings.Contains(m.Content, "was killed by the user") {
				return true
			}
		}
		return false
	})
	if ids := registeredRuns(t, s); len(ids) != 2 {
		t.Fatalf("%d runs total, want exactly 2 (user + user, no wake)", len(ids))
	}
}

// Scenario D1 counterpart: a model-initiated KillShell keeps today's
// behaviour — the attributed notification is queued wake-worthy and the
// driver starts exactly one auto run that carries it. The final model call
// is held open (perCallDelay) so the kill's completion queues while the run
// is still busy — before its last queue drain has passed — and the clean
// finish wakes, mirroring the coalescing scenario's timing.
func TestModelKilledShellStillWakes(t *testing.T) {
	reg := tools.NewRegistry(t.TempDir())
	kill := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
		ID:   fmt.Sprintf("kill-%d", wakeToolSeq.Add(1)),
		Name: "KillShell",
		Args: map[string]any{"shell_id": "1"},
	}}}
	// Cycle: arm the shell, kill it via the tool, ack, then the wake reply.
	a := newWakeAgentReg(t, 300*time.Millisecond, reg,
		bgShellCall("sleep 30"),
		kill,
		messages.AI("killed it"),
		messages.AI("noted"),
	)
	s, ts := newWakeServer(t, a, nil)

	runID, sid, _ := postRun(t, ts.URL, "s1", "start and kill the shell")
	waitRunDoneInProc(t, s, runID)

	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	if wakeID == runID {
		t.Fatalf("second registered run is the user run itself (%s)", wakeID)
	}
	waitRunDoneInProc(t, s, wakeID)
	wakeData := runStartedData(t, replayRunEvents(t, ts.URL, wakeID))
	if auto, _ := wakeData["auto"].(bool); !auto {
		t.Fatalf("wake run run_started data = %v, want auto:true", wakeData)
	}
	waitForTranscript(t, ts.URL, sid, "model-kill notification", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "background-task-completed") &&
				strings.Contains(m.Content, "was killed (KillShell)") {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs after settle, want exactly 2", n)
	}
}

// Scenario R4 (carried fix): the lost-wakeup window between wakeAfterRun's
// check and the admission release. run.onFinish fires BEFORE onDone releases
// the gate, so a notification queued in between — here via the store's
// release-timing test hook, which runs inside the completion callback after
// the save but before the release — saw admission.busy and returned without
// waking, and nothing re-checked: the item waited for the next user run.
// The completion path re-checks once the gate is open, keeping the usual
// chain/stall/cap/tombstone rules.
func TestWakeRechecksAfterAdmissionRelease(t *testing.T) {
	a := newWakeAgent(t, 0,
		messages.AI("ok"),
		messages.AI("noted"),
	)
	s, ts := newWakeServer(t, a, nil)

	// testAfterSave fires at the exact point of the window: onFinish (and
	// with it wakeAfterRun, which found an empty queue) has already run, the
	// gate is still held. One-shot: the wake run's own save must not re-arm.
	var armed atomic.Bool
	s.sessions.testAfterSave = func(id string) {
		if armed.CompareAndSwap(false, true) {
			a.QueueNotificationFor(id, "background-task-completed",
				"Background shell 9 finished (exit code 0): echo r4\nOutput (tail):\nr4")
		}
	}

	runID, sid, _ := postRun(t, ts.URL, "s1", "go")
	waitRunDoneInProc(t, s, runID)

	// Without the re-check the notification stays queued and no second run
	// ever appears; with it the release wakes the session exactly once.
	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	if wakeID == runID {
		t.Fatalf("second registered run is the user run itself (%s)", wakeID)
	}
	waitRunDoneInProc(t, s, wakeID)
	if d := runStartedData(t, replayRunEvents(t, ts.URL, wakeID)); func() bool {
		b, _ := d["auto"].(bool)
		return !b
	}() {
		t.Fatalf("wake run run_started = %v, want auto:true", d)
	}
	waitForTranscript(t, ts.URL, sid, "late notification delivered", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "<background-task-completed>") &&
				strings.Contains(m.Content, "r4") {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs after settle, want exactly 2", n)
	}
	if n := a.QueuedInputCountFor(sid); n != 0 {
		t.Fatalf("queued count = %d, want 0 (the notification was delivered)", n)
	}
}
