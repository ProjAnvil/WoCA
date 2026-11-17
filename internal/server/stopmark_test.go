package server

// F1 stop-notice persistence: a cancelled run must persist a derived history
// (the round's prompt plus a `<run-stopped run="..." reason="..."/>` notice
// tag) instead of losing everything on reload. The cases cover the two
// derivation formulas (main run with a human turn, goal run without), the
// clone-before-append aliasing hazard (sessionStore.load returns a slice
// shared with the in-memory cache), the two reason attributes, and the four
// cancel seams: user cancel, cancel while queued (both run kinds), and the
// parked-interrupt TTL timeout.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// wantStopNotice is the exact expected persisted notice content.
func wantStopNotice(runID, reason string) string {
	return `<run-stopped run="` + runID + `" reason="` + reason + `"/>`
}

// lastStopNotice returns the last message when it is the stop notice, else "".
func lastStopNotice(msgs []messages.Message) string {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != messages.RoleHuman {
		return ""
	}
	c := msgs[len(msgs)-1].Content
	if strings.HasPrefix(c, "<run-stopped") {
		return c
	}
	return ""
}

// awaitSave blocks for n completed saves (receipts come from trackSaves,
// runscope_test.go's save hook). The save trails the terminal event, so a
// drained receipt proves the async transcript write reached disk before the
// test ends — see the deviation notes in sessionstate_sse_test.go.
func awaitSave(t *testing.T, saved <-chan string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-saved:
		case <-time.After(10 * time.Second):
			t.Fatal("transcript save not completed")
		}
	}
}

// Case 1: the main-run derivation formula clones the base history (an append
// to the shared backing array must not leak into the derived transcript) and
// appends the human prompt plus the notice tag.
func TestDerivedCancelledHistoryMainRunFormulaClonesBase(t *testing.T) {
	history := make([]messages.Message, 2, 8) // spare capacity: a plain append would alias
	history[0] = messages.Human("earlier question")
	history[1] = messages.AI("earlier answer")
	human := messages.Human("cancelled prompt")

	derived := derivedCancelledHistory(history, &human, "run-1", StopReasonUser)

	if len(derived) != 4 {
		t.Fatalf("derived len = %d, want 4 (history + human + tag): %+v", len(derived), derived)
	}
	if got := derived[2]; got.Role != messages.RoleHuman || got.Content != "cancelled prompt" {
		t.Fatalf("derived[2] = %+v, want the human prompt", got)
	}
	if got := derived[3]; got.Role != messages.RoleHuman || got.Content != wantStopNotice("run-1", StopReasonUser) {
		t.Fatalf("derived[3] = %+v, want the stop notice tag", got)
	}

	// Aliasing (spec M4): sessionStore.load returns a slice shared with the
	// in-memory session state. The next append to that base (e.g. the next
	// run's prompt ride-along) must not overwrite the derived transcript's
	// stored human — it would, if the derivation appended without cloning.
	next := append(history, messages.Human("next run prompt"))
	_ = next
	if got := derived[2].Content; got != "cancelled prompt" {
		t.Fatalf("append to the base history overwrote the derived prompt: %q", got)
	}
	if len(history) != 2 {
		t.Fatalf("base history length changed: %d", len(history))
	}
}

// Case 2: the goal-run derivation (nil human — the goal round has no human
// turn) appends only the tag; no human is duplicated.
func TestDerivedCancelledHistoryGoalFormulaWithoutHuman(t *testing.T) {
	history := []messages.Message{
		messages.Human("set the goal"),
		messages.AI("cycle reply"),
	}
	baseHumans := 0
	for _, m := range history {
		if m.Role == messages.RoleHuman {
			baseHumans++
		}
	}
	derived := derivedCancelledHistory(history, nil, "run-2", StopReasonParkedTTL)
	if len(derived) != 3 {
		t.Fatalf("derived len = %d, want 3 (history + tag): %+v", len(derived), derived)
	}
	humans := 0
	for _, m := range derived {
		if m.Role == messages.RoleHuman {
			humans++
		}
	}
	if humans != baseHumans+1 {
		t.Fatalf("derived carries %d humans, want %d + the tag alone (no duplicated human turn)", humans, baseHumans)
	}
	if got := lastStopNotice(derived); got != wantStopNotice("run-2", StopReasonParkedTTL) {
		t.Fatalf("derived tail = %q, want the stop notice tag", got)
	}
}

// Case 3: the notice tag carries both reason attributes verbatim.
func TestStopNoticeReasonAttributes(t *testing.T) {
	if got := stopNotice("run-9", StopReasonUser).Content; got != wantStopNotice("run-9", StopReasonUser) {
		t.Fatalf("user stop notice = %q", got)
	}
	if got := stopNotice("run-9", StopReasonParkedTTL).Content; got != wantStopNotice("run-9", StopReasonParkedTTL) {
		t.Fatalf("parked TTL notice = %q", got)
	}
	if m := stopNotice("r", StopReasonUser); m.Role != messages.RoleHuman {
		t.Fatalf("notice role = %v, want Human (notification convention)", m.Role)
	}
}

// Case 4 (user stop, main run): cancelling an in-flight run persists the
// prompt plus the notice tag with reason=user_stop; no partial reply.
func TestUserCancelledRunPersistsStopNotice(t *testing.T) {
	a := newWakeAgent(t, 2*time.Second, messages.AI("late reply")) // model call held open for the cancel
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)

	runID, sid, _ := postRun(t, ts.URL, "s1", "my important prompt")
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "stop notice after cancel", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonUser)
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 2 {
		t.Fatalf("transcript = %+v, want exactly [prompt, notice]", msgs)
	}
	if msgs[0].Role != messages.RoleHuman || msgs[0].Content != "my important prompt" {
		t.Fatalf("transcript[0] = %+v, want the persisted prompt", msgs[0])
	}
	for _, m := range msgs {
		if m.Role == messages.RoleAI {
			t.Fatalf("cancelled round persisted a partial reply: %+v", msgs)
		}
	}
}

// Case 4 (queued run): a run cancelled while waiting on the session gate
// never reaches onDone; its prompt plus notice must persist from the queued
// branch. The gate is held by a synthetic holder so no other run's save can
// race the assertion.
func TestQueuedCancelledRunPersistsStopNotice(t *testing.T) {
	a := newWakeAgent(t, 0, messages.AI("never started"))
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)
	const sid = "s1"

	if w, pos, ok := s.admission.enter(sid, nil); w != nil || pos != 0 || !ok {
		t.Fatalf("synthetic holder enter = waiter %v pos %d ok %v", w != nil, pos, ok)
	}
	t.Cleanup(func() { s.admission.release(sid) })

	runID, _, pos := postRun(t, ts.URL, sid, "queued prompt")
	if pos != 1 {
		t.Fatalf("queue position = %d, want 1 (queued behind the holder)", pos)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "stop notice after queued cancel", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonUser)
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 2 || msgs[0].Content != "queued prompt" {
		t.Fatalf("transcript = %+v, want exactly [queued prompt, notice]", msgs)
	}
}

// Case 4 (parked TTL, run.go's waitParked timer): a run parked on an
// unanswered interrupt is cancelled by the TTL and persists the notice with
// reason=parked_ttl (distinct from a user stop).
func TestParkedTTLRunPersistsStopNoticeWithParkedReason(t *testing.T) {
	old := parkedRunTTL
	parkedRunTTL = 150 * time.Millisecond
	defer func() { parkedRunTTL = old }()

	// The first model turn parks on an ask-user interrupt that nothing
	// answers (newTestAgent's shape; ModeDefault so the ask really parks).
	s, ts := newWakeServer(t, newTestAgent(t), nil)
	saved := trackSaves(s)

	runID, sid, _ := postRun(t, ts.URL, "s1", "awaiting an answer")
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "parked_ttl stop notice", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonParkedTTL)
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 2 || msgs[0].Content != "awaiting an answer" {
		t.Fatalf("transcript = %+v, want exactly [prompt, notice]", msgs)
	}
}

// newStopmarkGoalServer serves a goal-capable agent whose assess call is held
// open by the rate limiter, so the goal run can be cancelled mid-cycle.
func newStopmarkGoalServer(t *testing.T, perCallDelay time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	opts := []language.FakeChatModelOption{
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("criteria checklist"),                       // assess (held open)
			messages.AI("implemented"),                              // execute
			messages.AI(`{"verdict":"complete","evidence":"done"}`), // evaluate (unreached)
		),
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
	dir := t.TempDir()
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: dir, WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// Goal cancel (executing): the goal round has no human turn, so the derived
// history is the session history plus the tag alone — no duplicated human.
func TestGoalCancelledRunPersistsStopNoticeWithoutHuman(t *testing.T) {
	s, ts := newStopmarkGoalServer(t, 2*time.Second)
	saved := trackSaves(s)
	const sid = "s-goal"

	_, runID, output := postGoal(t, ts.URL, sid, "set make the feature work")
	if runID == "" {
		t.Fatalf("goal set output = %q, want a started run", output)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "goal stop notice", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonUser)
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 1 {
		t.Fatalf("transcript = %+v, want exactly [notice] (goal rounds add no human turn)", msgs)
	}
}

// Goal cancel while queued: same derivation, no human turn, straight from the
// queued branch (onDone never runs).
func TestGoalQueuedCancelledRunPersistsStopNotice(t *testing.T) {
	s, ts := newStopmarkGoalServer(t, 0)
	saved := trackSaves(s)
	const sid = "s-goal-q"

	if w, pos, ok := s.admission.enter(sid, nil); w != nil || pos != 0 || !ok {
		t.Fatalf("synthetic holder enter = waiter %v pos %d ok %v", w != nil, pos, ok)
	}
	t.Cleanup(func() { s.admission.release(sid) })

	_, runID, _ := postGoal(t, ts.URL, sid, "set make the feature work")
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "queued goal stop notice", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonUser)
	})
	if msgs := sessionMessages(t, ts.URL, sid); len(msgs) != 1 {
		t.Fatalf("transcript = %+v, want exactly [notice]", msgs)
	}
}

// A clean completion keeps today's transcript: the reply persists and no stop
// notice appears.
func TestCleanRunPersistsNoStopNotice(t *testing.T) {
	a := newWakeAgent(t, 0, messages.AI("all done"))
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)

	runID, sid, _ := postRun(t, ts.URL, "s1", "plain question")
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "clean reply", func(msgs []messages.Message) bool {
		return len(msgs) == 2 && msgs[1].Role == messages.RoleAI && msgs[1].Content == "all done"
	})
	for _, m := range sessionMessages(t, ts.URL, sid) {
		if strings.HasPrefix(m.Content, "<run-stopped") {
			t.Fatalf("clean run persisted a stop notice: %+v", m)
		}
	}
}
