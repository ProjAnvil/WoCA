package server

// Goal-completion wake behavior (F3 item 3), pinned as deliberate.
//
// Since the completeRun fold, a CLEAN goal completion passes through
// completeRun's post-release wake re-check (maybeWake) — the R4 lost-wakeup
// guard the normal-run path has. The machinery is NOT identical to normal
// runs, and the difference is the pinned contract:
//
//   - normal runs wake from TWO seams: wakeAfterRun in onFinish (pre-release,
//     no busy re-check — the wake queues behind the finishing run's gate) and
//     completeRun's maybeWake after the release (R4 re-check);
//   - goal runs never had the onFinish seam (a goal round is not a user turn
//     handing the session back); their ONLY wake seam is completeRun's
//     maybeWake, gated on ranClean exactly like a normal run's.
//
// So: a notification that lands while the session is still gated (here via
// the completion-save test hook — inside completeRun, after onFinish, before
// the release: the exact window where no observer fires and only the
// re-check can deliver) is delivered by one auto-annotated wake run after a
// clean goal completion, and is NOT delivered after a cancelled one (the
// ranClean rule: never restart over what the user just stopped).

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newGoalWakeAgent builds an agent whose model runs one scripted goal cycle
// to a verified completion, plus (when wakeReply is set) one extra response
// for the wake run the completion is expected to start.
func newGoalWakeAgent(t *testing.T, perCallDelay time.Duration, wakeReply string) *agent.Agent {
	t.Helper()
	responses := []messages.Message{
		messages.AI("criteria checklist\nThis cycle: implement"), // assess
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
			ID: "1", Name: "Read", Args: map[string]any{"file_path": "x"},
		}}},
		messages.AI("implemented"),                                   // execute final
		messages.AI(`{"verdict":"complete","evidence":"all green"}`), // evaluate
	}
	if wakeReply != "" {
		responses = append(responses, messages.AI(wakeReply))
	}
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// armCompletionNotification queues one wakeable notification the first time
// the session's transcript saves — inside completeRun's save, i.e. after the
// terminal event and onFinish, before the admission release. The session is
// still gated there, so the queued-notification observer stays silent and
// only the completion path's wake re-check can deliver the item.
func armCompletionNotification(t *testing.T, s *Server, sid, text string) {
	t.Helper()
	var armed atomic.Bool
	s.sessions.testAfterSave = func(id string) {
		if id != sid || !armed.CompareAndSwap(false, true) {
			return
		}
		s.agent.QueueNotificationFor(id, "background-task-completed", text)
	}
}

// Clean goal completion: the notification queued in the completion window is
// delivered by exactly one wake run (auto-annotated), and nothing beyond it
// starts.
func TestGoalCompletionRechecksWakeAfterGateRelease(t *testing.T) {
	a := newGoalWakeAgent(t, 0, "noted")
	s, ts := newWakeServer(t, a, nil)
	const sid = "sess-goal-wake"
	armCompletionNotification(t, s, sid,
		"Background shell 9 finished (exit code 0): echo goalwake\nOutput (tail):\ngoalwake")

	_, runID, _ := postGoal(t, ts.URL, sid, "set make the feature work")
	waitRunDoneInProc(t, s, runID)

	// The re-check starts the wake run; without it the notification stays
	// queued and no second run ever registers.
	ids := waitForRuns(t, s, 2)
	wakeID := ids[len(ids)-1]
	if wakeID == runID {
		t.Fatalf("second registered run is the goal run itself (%s)", wakeID)
	}
	waitRunDoneInProc(t, s, wakeID)
	if d := runStartedData(t, replayRunEvents(t, ts.URL, wakeID)); func() bool {
		b, _ := d["auto"].(bool)
		return !b
	}() {
		t.Fatalf("wake run run_started = %v, want auto:true", d)
	}

	// The notification reached the transcript through the wake turn.
	waitForTranscript(t, ts.URL, sid, "goal notification delivered", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "<background-task-completed>") &&
				strings.Contains(m.Content, "goalwake") {
				return true
			}
		}
		return false
	})
	if n := a.QueuedInputCountFor(sid); n != 0 {
		t.Fatalf("queued count = %d, want 0 (the notification was delivered)", n)
	}

	// Settle: exactly the goal run and its single wake run — no chain.
	time.Sleep(250 * time.Millisecond)
	if n := len(registeredRuns(t, s)); n != 2 {
		t.Fatalf("%d runs after settle, want exactly 2", n)
	}
}

// Cancelled goal completion: the same completion-window notification stays
// queued — the ranClean rule keeps a cancelled round from handing the session
// back to the wake driver, so the item waits for the next user run.
func TestCancelledGoalRunDoesNotWake(t *testing.T) {
	a := newGoalWakeAgent(t, 300*time.Millisecond, "")
	s, ts := newWakeServer(t, a, nil)
	const sid = "sess-goal-cancel"
	armCompletionNotification(t, s, sid,
		"Background shell 9 finished (exit code 0): echo goalcancel")

	_, runID, _ := postGoal(t, ts.URL, sid, "set make the feature work")
	// Cancel mid-assess (the delayed model call keeps the run in flight).
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	if reason, cancelled := s.lookupRun(runID).cancelledReason(); !cancelled {
		t.Fatalf("goal run not cancelled (reason=%q)", reason)
	}

	// The cancelled completion still persists its derived history (the hook
	// fired and queued the notification while the gate was held); nothing
	// may deliver it now.
	waitForQueueCount(t, a, sid, 1)
	time.Sleep(250 * time.Millisecond)
	if n := a.QueuedInputCountFor(sid); n != 1 {
		t.Fatalf("queued count = %d, want 1 (a cancelled goal run must not wake)", n)
	}
	if ids := registeredRuns(t, s); len(ids) != 1 {
		t.Fatalf("%d runs after cancelled goal completion, want 1 (no wake): %v", len(ids), fmt.Sprint(ids))
	}
}
