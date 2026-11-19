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
	"context"
	"net/http"
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

	derived := derivedCancelledHistory(history, &human, nil, nil, "run-1", StopReasonUser)

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
	derived := derivedCancelledHistory(history, nil, nil, nil, "run-2", StopReasonParkedTTL)
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

// Case 2b (spec 2026-11-19-cancel-partial-output §D3): the partial and synth
// segments land between the human prompt and the stop notice, in stream
// order. The steers scenario pins the Human-after-tools invariant: mid-turn
// user input sits after a completed tool batch and before the next model
// turn, so a dangling AI message is always the partial's LAST message — the
// appended synth keeps every tool_use/result pairing legal.
func TestDerivedCancelledHistoryWithPartialAndSynth(t *testing.T) {
	history := []messages.Message{messages.Human("earlier"), messages.AI("earlier reply")}
	human := messages.Human("run it")
	ai1 := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "c1", Name: "Bash"}}}
	res1 := messages.Tool("c1", "ok")
	steer := messages.Human("<queued-user-input>more context</queued-user-input>")
	ai2 := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "c2", Name: "Bash"}}}
	partial := []messages.Message{ai1, res1, steer, ai2}

	synth := synthesizeAborted(partial, StopReasonUser, 3*time.Second)
	if len(synth) != 1 || synth[0].ToolCallID != "c2" {
		t.Fatalf("synth = %+v, want one result for the dangling c2 only (c1 is answered)", synth)
	}

	derived := derivedCancelledHistory(history, &human, partial, synth, "run-3", StopReasonUser)
	if len(derived) != 9 {
		t.Fatalf("derived len = %d, want 9 (2 base + human + 4 partial + synth + notice): %+v", len(derived), derived)
	}
	wantOrder := []struct {
		role messages.Role
		id   string
	}{
		{messages.RoleHuman, ""},
		{messages.RoleAI, ""},     // ai1
		{messages.RoleTool, "c1"}, // res1
		{messages.RoleHuman, ""},  // steer
		{messages.RoleAI, ""},     // ai2 (dangling tail)
		{messages.RoleTool, "c2"}, // synth
	}
	for i, w := range wantOrder {
		got := derived[2+i]
		if got.Role != w.role || got.ToolCallID != w.id {
			t.Fatalf("derived[%d] = %+v, want role %s id %q", 2+i, got, w.role, w.id)
		}
	}
	if got := lastStopNotice(derived); got != wantStopNotice("run-3", StopReasonUser) {
		t.Fatalf("derived tail = %q, want the stop notice tag", got)
	}
	// Clone posture: a later append to the partial slice must not leak into
	// the derived transcript.
	partial = append(partial, messages.AI("late"))
	if got := derived[7]; got.Role != messages.RoleTool || got.ToolCallID != "c2" {
		t.Fatalf("append to the partial leaked into derived: %+v", derived[7])
	}
}

// Case 2c: nil partial/synth (queued cancels — the run never executed;
// nothing streamed) keeps the two-segment formula byte-for-byte.
func TestDerivedCancelledHistoryWithoutPartialKeepsFormula(t *testing.T) {
	history := []messages.Message{messages.Human("q")}
	human := messages.Human("cancelled prompt")
	derived := derivedCancelledHistory(history, &human, nil, nil, "run-4", StopReasonUser)
	if len(derived) != 3 {
		t.Fatalf("derived len = %d, want 3 (history + human + notice): %+v", len(derived), derived)
	}
	if got := derived[2]; got.Role != messages.RoleHuman || got.Content != wantStopNotice("run-4", StopReasonUser) {
		t.Fatalf("derived[2] = %+v, want the stop notice tag", got)
	}
}

// Case 3: the notice tag carries every stop cause's reason attribute
// verbatim — the closed cause set round-trips through StopNotice.
func TestStopNoticeReasonAttributes(t *testing.T) {
	for _, cause := range []string{StopReasonUser, StopReasonParkedTTL, StopReasonServerStop, StopReasonLeaseLost} {
		if got := stopNotice("run-9", cause).Content; got != wantStopNotice("run-9", cause) {
			t.Fatalf("%s notice = %q", cause, got)
		}
	}
	if m := stopNotice("r", StopReasonUser); m.Role != messages.RoleHuman {
		t.Fatalf("notice role = %v, want Human (notification convention)", m.Role)
	}
}

// Case 4 (user stop, main run): cancelling an in-flight run persists the
// prompt plus the notice tag with reason=user_stop; no partial reply. The
// done event carries the same cause (live UI gap 2: the SSE payload, not
// the reload, tells the frontend why the run stopped).
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
	// The done payload carries the cause next to cancelled:true.
	for _, f := range replayRunEvents(t, ts.URL, runID) {
		if f.event != EventDone {
			continue
		}
		if !strings.Contains(f.data, `"cancelled":true`) || !strings.Contains(f.data, `"reason":"`+StopReasonUser+`"`) {
			t.Fatalf("done payload = %s, want cancelled:true reason user_stop", f.data)
		}
	}
}

// Case 4 (queued run, clobber regression — cancel-semantics gap 1): a run
// cancelled while waiting on the session gate never reaches onDone; its
// prompt plus notice must persist — AND survive the in-flight holder's
// completion save. The holder's save is a full overwrite built from the
// history it loaded at start, so an immediate queued-cancel save (written
// outside the admission window) was clobbered by it. The queued cancel is
// therefore deferred: the holder's completion drains it between its own
// save and the gate release, so BOTH transcripts persist, in order.
func TestQueuedCancelWhileHolderRunsPersistsBoth(t *testing.T) {
	a := newWakeAgent(t, 2*time.Second, messages.AI("holder reply")) // holder's model call held open across the cancel
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)
	const sid = "s1"

	runA, _, posA := postRun(t, ts.URL, sid, "holder prompt")
	if posA != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", posA)
	}
	runB, _, posB := postRun(t, ts.URL, sid, "queued prompt")
	if posB != 1 {
		t.Fatalf("queued position = %d, want 1 (queued behind the holder)", posB)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+runB+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runB)
	waitRunDoneInProc(t, s, runA)
	awaitSave(t, saved, 2) // holder's final transcript + the drained queued-cancel save

	waitForTranscript(t, ts.URL, sid, "holder reply AND queued prompt+notice", func(msgs []messages.Message) bool {
		return len(msgs) == 4 &&
			msgs[0].Content == "holder prompt" &&
			msgs[1].Role == messages.RoleAI && msgs[1].Content == "holder reply" &&
			msgs[2].Content == "queued prompt" &&
			lastStopNotice(msgs) == wantStopNotice(runB, StopReasonUser)
	})
}

// Case 4 (parked TTL, run.go's waitParked timer): a run parked on an
// unanswered interrupt is cancelled by the TTL and persists the notice with
// reason=parked_ttl (distinct from a user stop). The parked ask's committed
// tool call is dangling — the partial transcript keeps the AI message and a
// synthesized terminal result answers the call ahead of the notice (spec
// 2026-11-19-cancel-partial-output §D2/§D3).
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
	if len(msgs) != 4 || msgs[0].Content != "awaiting an answer" {
		t.Fatalf("transcript = %+v, want [prompt, partial ask, synth, notice]", msgs)
	}
	if got := msgs[1]; got.Role != messages.RoleAI || len(got.ToolCalls) != 1 {
		t.Fatalf("partial = %+v, want the committed AskUserQuestion call", got)
	}
	if got := msgs[2]; got.Role != messages.RoleTool || got.ToolCallID != msgs[1].ToolCalls[0].ID ||
		!strings.HasPrefix(got.Content, "Tool call aborted: approval timed out") {
		t.Fatalf("synth = %+v, want the parked_ttl abort wording", got)
	}
	// The done payload carries parked_ttl, not the generic user_stop.
	for _, f := range replayRunEvents(t, ts.URL, runID) {
		if f.event != EventDone {
			continue
		}
		if !strings.Contains(f.data, `"reason":"`+StopReasonParkedTTL+`"`) {
			t.Fatalf("done payload = %s, want reason parked_ttl", f.data)
		}
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

// Multiple queued cancels drain in FIFO order: both cancelled runs' prompts
// and notices persist after the holder's transcript, in cancel order, each
// finalizer rebasing on the previous writes.
func TestMultipleQueuedCancelsDrainInOrder(t *testing.T) {
	a := newWakeAgent(t, 2*time.Second, messages.AI("holder reply"))
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)
	const sid = "s1"

	runA, _, _ := postRun(t, ts.URL, sid, "holder prompt")
	runB1, _, posB1 := postRun(t, ts.URL, sid, "queued one")
	runB2, _, posB2 := postRun(t, ts.URL, sid, "queued two")
	if posB1 != 1 || posB2 != 2 {
		t.Fatalf("queue positions = %d/%d, want 1/2", posB1, posB2)
	}
	for _, id := range []string{runB1, runB2} {
		if err := postJSON(t, ts.URL+"/api/runs/"+id+"/cancel", ""); err != nil {
			t.Fatal(err)
		}
		waitRunDoneInProc(t, s, id)
	}
	waitRunDoneInProc(t, s, runA)
	awaitSave(t, saved, 3) // holder's final + two drained finalizers

	waitForTranscript(t, ts.URL, sid, "holder reply, both prompts+notices in order", func(msgs []messages.Message) bool {
		return len(msgs) == 6 &&
			msgs[1].Content == "holder reply" &&
			msgs[2].Content == "queued one" &&
			lastStopNotice(msgs[:4]) == wantStopNotice(runB1, StopReasonUser) &&
			msgs[4].Content == "queued two" &&
			lastStopNotice(msgs) == wantStopNotice(runB2, StopReasonUser)
	})
}

// Goal cancel while queued (clobber regression, goal twin): the goal run
// queued behind an ordinary in-flight run on the same session is cancelled;
// its notice (no human turn) must persist after the holder's own transcript,
// through the same deferred finalizer drain.
func TestGoalQueuedCancelBehindHolderPersistsBoth(t *testing.T) {
	s, ts := newStopmarkGoalServer(t, 2*time.Second) // holder's model call held open across the cancel
	saved := trackSaves(s)
	const sid = "s-goal-q"

	// The gate holder is an ordinary run (a second goal loop cannot start —
	// the agent-level goal claim refuses it — so goal runs queue only behind
	// normal runs).
	runA, _, posA := postRun(t, ts.URL, sid, "holder prompt")
	if posA != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", posA)
	}
	_, runB, output := postGoal(t, ts.URL, sid, "set make the feature work")
	if runB == "" {
		t.Fatalf("goal set output = %q, want a queued goal run", output)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+runB+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runB)
	waitRunDoneInProc(t, s, runA)
	awaitSave(t, saved, 2) // holder's final transcript + the drained goal queued-cancel save

	waitForTranscript(t, ts.URL, sid, "holder reply AND goal notice", func(msgs []messages.Message) bool {
		return len(msgs) == 3 &&
			msgs[0].Content == "holder prompt" &&
			msgs[1].Role == messages.RoleAI &&
			lastStopNotice(msgs) == wantStopNotice(runB, StopReasonUser)
	})
}

// Shutdown abort (server_stop): admission.stop() aborts queued waiters with
// the server_stop cause — not user_stop — and the shutdown flush persists
// their derived history (no holder will drain the finalizer queue; in-flight
// runs are not cancelled at shutdown). The done event carries the same
// cause as the persisted tag.
func TestShutdownAbortedQueuedRunPersistsServerStop(t *testing.T) {
	a := newWakeAgent(t, 0, messages.AI("unused"))
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)
	const sid = "s1"

	// A synthetic holder keeps the gate busy so the second run queues (and
	// stays queued — nothing cancels the holder here).
	if w, pos, ok := s.admission.enter(sid, nil); w != nil || pos != 0 || !ok {
		t.Fatalf("synthetic holder enter = waiter %v pos %d ok %v", w != nil, pos, ok)
	}
	t.Cleanup(func() { s.admission.release(sid) })

	runID, _, pos := postRun(t, ts.URL, sid, "queued prompt")
	if pos != 1 {
		t.Fatalf("queue position = %d, want 1 (queued behind the holder)", pos)
	}

	// Server shutdown: ServeStarted stops admission (aborting the queued
	// waiter) and flushes the finalizer queue.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ServeStarted(ctx, &http.Server{}, nil); err != nil {
		t.Fatalf("ServeStarted after cancel = %v, want nil", err)
	}

	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)
	waitForTranscript(t, ts.URL, sid, "server_stop notice after shutdown", func(msgs []messages.Message) bool {
		return lastStopNotice(msgs) == wantStopNotice(runID, StopReasonServerStop)
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 2 || msgs[0].Content != "queued prompt" {
		t.Fatalf("transcript = %+v, want exactly [queued prompt, notice]", msgs)
	}
	for _, f := range replayRunEvents(t, ts.URL, runID) {
		if f.event != EventDone {
			continue
		}
		if !strings.Contains(f.data, `"cancelled":true`) || !strings.Contains(f.data, `"reason":"`+StopReasonServerStop+`"`) {
			t.Fatalf("done payload = %s, want cancelled:true reason server_stop", f.data)
		}
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
