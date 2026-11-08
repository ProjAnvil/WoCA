package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// The buffer-mechanics pins (add/drain/count semantics, session scoping,
// concurrent add, the addTagged-under-lock atomicity) moved to the queue
// subpackage when it was extracted (queue-extract spec); what remains here
// is the agent wiring: run-start prepend, the toolsNode attach, session
// delivery end to end, and the released-session interplay.

// newQueueTestAgent builds an agent whose model requests one Sleep call then
// finishes, using the shared recorder-based Sleep tool.
func newQueueTestAgent(t *testing.T, rec *recorder, sleepMS int) *Agent {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Sleep", rec))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "q1", Name: "Sleep", Args: map[string]any{"ms": sleepMS}},
			}},
			messages.AI("all done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestQueuedInputInjectedAfterToolResults(t *testing.T) {
	rec := &recorder{}
	a := newQueueTestAgent(t, rec, 300)

	// Input queued before a run starts is delivered at run start
	// (prependQueued); the after-tool-results attachment only applies to
	// input arriving mid-run, so submit it while the tool is still sleeping.
	type result struct {
		out []messages.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
		done <- result{out, err}
	}()
	time.Sleep(80 * time.Millisecond)
	a.QueueInput("actually, also check the tests")

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	out := res.out

	var toolIdx, queuedIdx, finalIdx = -1, -1, -1
	for i, m := range out {
		switch {
		case m.Role == messages.RoleTool && m.ToolCallID == "q1":
			toolIdx = i
		case m.Role == messages.RoleHuman && strings.Contains(m.Content, "<queued-user-input>") &&
			strings.Contains(m.Content, "actually, also check the tests"):
			queuedIdx = i
		case m.Role == messages.RoleAI && m.Content == "all done":
			finalIdx = i
		}
	}
	if toolIdx == -1 || queuedIdx == -1 || finalIdx == -1 {
		t.Fatalf("transcript missing pieces: tool=%d queued=%d final=%d (%+v)", toolIdx, queuedIdx, finalIdx, out)
	}
	// The queued input is attached strictly after the tool results (API
	// ordering) and before the model's next response.
	if queuedIdx < toolIdx || finalIdx < queuedIdx {
		t.Fatalf("ordering wrong: tool=%d queued=%d final=%d", toolIdx, queuedIdx, finalIdx)
	}
	if a.QueuedInputCount() != 0 {
		t.Fatal("queue not drained")
	}
}

func TestQueuedInputDuringToolExecution(t *testing.T) {
	rec := &recorder{}
	a := newQueueTestAgent(t, rec, 300)

	type result struct {
		out []messages.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
		done <- result{out, err}
	}()

	// Submit while the tool is still sleeping.
	time.Sleep(80 * time.Millisecond)
	a.QueueInput("mid-turn correction")

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	var sawQueued bool
	for _, m := range res.out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "mid-turn correction") {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatal("input submitted during tool execution was not attached")
	}
	if rec.count() != 1 {
		t.Fatalf("Sleep ran %d times, want 1", rec.count())
	}
}

func TestNoQueuedInputNoExtraMessages(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 10)
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run the check")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "<queued-user-input>") {
			t.Fatal("unexpected queued-input message with an empty queue")
		}
	}
	// system, human, AI(toolcall), tool, AI = 5.
	if len(out) != 5 {
		t.Fatalf("transcript length = %d: %+v", len(out), out)
	}
}

// TestQueueNotificationForSessionDelivery verifies session-scoped delivery
// end to end: with notifications queued for sessions "a" and "b", a run whose
// ctx carries tracing.WithRun(ctx, "r1", "a") receives only the "a" one —
// delivered at run start, ahead of the fresh user turn — and the "b" item
// stays queued for its own session.
func TestQueueNotificationForSessionDelivery(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 10)
	a.QueueNotificationFor("a", "bg-done", "result for session a")
	a.QueueNotificationFor("b", "bg-done", "result for session b")

	ctx := tracing.WithRun(t.Context(), "r1", "a")
	out, err := a.Invoke(ctx, []messages.Message{messages.Human("run the check")}, "t")
	if err != nil {
		t.Fatal(err)
	}

	var notifA, notifB, humanIdx = -1, -1, -1
	for i, m := range out {
		switch {
		case m.Role == messages.RoleHuman && strings.Contains(m.Content, "result for session a"):
			notifA = i
		case m.Role == messages.RoleHuman && strings.Contains(m.Content, "result for session b"):
			notifB = i
		case m.Role == messages.RoleHuman && m.Content == "run the check":
			humanIdx = i
		}
	}
	if notifA == -1 || humanIdx == -1 {
		t.Fatalf("transcript missing pieces: notifA=%d human=%d (%+v)", notifA, humanIdx, out)
	}
	if notifB != -1 {
		t.Fatalf("session-b notification leaked into session a's run at %d: %+v", notifB, out)
	}
	// Run-start delivery: the notification rides in ahead of the fresh user
	// turn instead of waiting for a tool round.
	if notifA > humanIdx {
		t.Fatalf("notification not prepended: notif=%d human=%d", notifA, humanIdx)
	}
	if got := a.QueuedInputCountFor("b"); got != 1 {
		t.Fatalf("queued for b = %d, want 1", got)
	}
	if got := a.QueuedInputCount(); got != 1 {
		t.Fatalf("total queued = %d, want 1", got)
	}
}

// TestQueueInputForScopedDelivery pins the web steer path's seam: input
// queued for session "a" is delivered to a's runs only — another session's
// drain never takes it — and DropScopedInputsFor (the wake driver's delete
// discard) removes exactly the session's own items, leaving unscoped and
// foreign items queued. QueueInput keeps its unscoped TUI semantics.
func TestQueueInputForScopedDelivery(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 1)
	a.QueueInputFor("a", "steer for a")

	if got := a.QueuedInputCountFor("a"); got != 1 {
		t.Fatalf("count for a = %d, want 1", got)
	}
	if got := a.QueuedInputCountFor("b"); got != 0 {
		t.Fatalf("count for b = %d, want 0 (b's drain never sees a's steer)", got)
	}
	if items := a.DrainQueuedInputsFor("b"); len(items) != 0 {
		t.Fatalf("drain for b = %+v, want nothing", items)
	}
	items := a.DrainQueuedInputsFor("a")
	if len(items) != 1 || items[0].Text != "steer for a" || items[0].Session != "a" || items[0].Tag != "" {
		t.Fatalf("drain for a = %+v, want the untagged a-scoped steer", items)
	}

	// The delete discard is exact-scope: dropping b's queue leaves a's items
	// (and unscoped ones) untouched.
	a.QueueInputFor("a", "another for a")
	a.QueueInputFor("b", "b only")
	a.QueueInput("tui note") // unscoped: any run may deliver it
	if n := a.DropScopedInputsFor("b"); n != 1 {
		t.Fatalf("DropScopedInputsFor(b) = %d, want 1", n)
	}
	// a's drain still sees a's own item plus the unscoped one; b's item is
	// gone but the unscoped TUI note remains visible to every session.
	items = a.DrainQueuedInputsFor("a")
	if len(items) != 2 ||
		(items[0].Text != "another for a" || items[0].Session != "a") ||
		(items[1].Text != "tui note" || items[1].Session != "") {
		t.Fatalf("drain for a after dropping b = %+v, want a's steer plus the unscoped note", items)
	}
	if got := a.QueuedInputCount(); got != 0 {
		t.Fatalf("queue not empty at the end = %d", got)
	}
}

// TestQueueInputForReleasedSessionRefused: a steer for a released (deleted)
// session is refused like any other queued item (D2) instead of lingering
// where nothing will ever drain it.
func TestQueueInputForReleasedSessionRefused(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 1)
	a.ReleaseSession("a")
	a.QueueInputFor("a", "late steer")
	if got := a.QueuedInputCountFor("a"); got != 0 {
		t.Fatalf("queued for released session = %d, want 0", got)
	}
}

// TestPrependQueuedInsertion pins where run-start queued input lands in the
// history: ahead of the trailing Human turn when there is one, appended at
// the end otherwise, and a no-op on an empty queue.
func TestPrependQueuedInsertion(t *testing.T) {
	a := newQueueTestAgent(t, &recorder{}, 1)
	ctx := tracing.WithRun(t.Context(), "r9", "s")

	a.QueueNotificationFor("s", "bg-done", "note one")
	a.QueueNotificationFor("s", "bg-done", "note two")
	ending := []messages.Message{messages.AI("prior"), messages.Human("fresh turn")}
	got := a.prependQueued(ctx, ending)
	if len(got) != 4 ||
		got[0].Content != "prior" ||
		got[1].Role != messages.RoleHuman || !strings.Contains(got[1].Content, "note one") ||
		got[2].Role != messages.RoleHuman || !strings.Contains(got[2].Content, "note two") ||
		got[3].Content != "fresh turn" {
		t.Fatalf("insertion ahead of trailing Human wrong: %+v", got)
	}

	// History not ending in Human appends at the end.
	a.QueueNotificationFor("s", "bg-done", "note three")
	noHuman := []messages.Message{messages.Human("old"), messages.AI("prior")}
	got = a.prependQueued(ctx, noHuman)
	if len(got) != 3 || !strings.Contains(got[2].Content, "note three") {
		t.Fatalf("append fallback wrong: %+v", got)
	}

	// An empty queue leaves the history untouched.
	fresh := []messages.Message{messages.Human("fresh turn")}
	if got := a.prependQueued(ctx, fresh); len(got) != 1 || got[0].Content != "fresh turn" {
		t.Fatalf("empty-queue passthrough wrong: %+v", got)
	}
}

// TestQueueNotificationForOtherSessionStaysQueuedMidRun covers the toolsNode
// attach point: a notification for session "b" arriving while session "a"'s
// run is mid-turn must not leak into "a"'s transcript — it stays queued for
// "b"'s own run.
func TestQueueNotificationForOtherSessionStaysQueuedMidRun(t *testing.T) {
	rec := &recorder{}
	a := newQueueTestAgent(t, rec, 300)

	type result struct {
		out []messages.Message
		err error
	}
	done := make(chan result, 1)
	ctx := tracing.WithRun(t.Context(), "r1", "a")
	go func() {
		out, err := a.Invoke(ctx, []messages.Message{messages.Human("run the check")}, "t")
		done <- result{out, err}
	}()

	// Arrives mid-turn, so only the toolsNode drain can pick it up.
	time.Sleep(80 * time.Millisecond)
	a.QueueNotificationFor("b", "bg-done", "result for session b")

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, m := range res.out {
		if strings.Contains(m.Content, "result for session b") {
			t.Fatalf("session-b notification leaked into session a's run: %+v", res.out)
		}
	}
	if got := a.QueuedInputCountFor("b"); got != 1 {
		t.Fatalf("queued for b = %d, want 1 (must survive a's run)", got)
	}
}
