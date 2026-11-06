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

// TestBackgroundCompletionNotificationInjected verifies the end-to-end
// background-shell path: a Bash run_in_background command finishes while other
// tools are still executing, and its completion notification is attached after
// the tool results with the background-task-completed tag (not the
// queued-user-input tag).
func TestBackgroundCompletionNotificationInjected(t *testing.T) {
	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Sleep", rec))
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "b1", Name: "Bash", Args: map[string]any{
					"command": "sleep 0.15; echo bg-output-42", "run_in_background": true,
				}},
				{ID: "s1", Name: "Sleep", Args: map[string]any{"ms": 400}},
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

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run things")}, "t")
	if err != nil {
		t.Fatal(err)
	}

	var bashIdx, notifIdx, finalIdx = -1, -1, -1
	for i, m := range out {
		switch {
		case m.Role == messages.RoleTool && m.ToolCallID == "s1":
			bashIdx = i // Sleep finishes last, so the notification must follow it
		case m.Role == messages.RoleHuman && strings.Contains(m.Content, "<background-task-completed>"):
			notifIdx = i
			if !strings.Contains(m.Content, "bg-output-42") {
				t.Fatalf("notification missing output: %q", m.Content)
			}
			// R4: the queue tag is the single wrapper; the producer body must
			// not embed its own (no nested identical tags).
			if strings.Count(m.Content, "<background-task-completed>") != 1 ||
				strings.Count(m.Content, "</background-task-completed>") != 1 {
				t.Fatalf("notification must carry exactly one wrapper, got: %q", m.Content)
			}
		case m.Role == messages.RoleAI && m.Content == "all done":
			finalIdx = i
		}
	}
	if bashIdx == -1 || notifIdx == -1 || finalIdx == -1 {
		t.Fatalf("transcript missing pieces: tool=%d notif=%d final=%d", bashIdx, notifIdx, finalIdx)
	}
	if notifIdx < bashIdx || finalIdx < notifIdx {
		t.Fatalf("notification ordering wrong: tool=%d notif=%d final=%d", bashIdx, notifIdx, finalIdx)
	}
	for _, m := range out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "<queued-user-input>") {
			t.Fatal("background notification should not use the queued-user-input tag")
		}
	}
}

// TestBackgroundNotificationIdleDeliveredNextRun verifies a completion that
// lands while the agent is idle stays queued and is attached on the next run's
// first tool round.
func TestBackgroundNotificationIdleDeliveredNextRun(t *testing.T) {
	rec := &recorder{}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(sleepToolNamed("Sleep", rec))
	responder := func() *language.FakeChatModel {
		return language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(
				messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "s1", Name: "Sleep", Args: map[string]any{"ms": 10}},
				}},
				messages.AI("done round"),
			),
		)
	}
	a, err := New(Config{Model: responder(), Registry: reg, SystemPrompt: "t", PermissionMode: config.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}

	// A shell completes between runs.
	if _, err := reg.Background().Start(t.Context(), "echo idle-77", "", "."); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a.QueuedInputCount() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a.QueuedInputCount() == 0 {
		t.Fatal("idle completion never queued")
	}

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("next run")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "idle-77") {
			found = true
		}
	}
	if !found {
		t.Fatal("idle notification not delivered on next run")
	}
}

// TestNotificationObserverQueuedBeforeRun covers the observer seam: a
// notification queued while no run is in flight fires the observer
// synchronously with the exact queued item (nil uninstalls it), and the next
// run — a plain-text model with no tool calls, so delivery can only come from
// the run-start prepend — shows the tagged Human messages ahead of the fresh
// user turn, each wrapped exactly once (the queue tag; R4 keeps the producer
// body wrapper-free).
func TestNotificationObserverQueuedBeforeRun(t *testing.T) {
	reg := tools.NewRegistry(t.TempDir())
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("plain answer")))
	a, err := New(Config{Model: model, Registry: reg, SystemPrompt: "t", PermissionMode: config.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}

	var got []QueuedInput
	a.SetNotificationObserver(func(q QueuedInput) { got = append(got, q) })

	const body = "Background shell b7 finished (exit code 0): echo observer-99"
	a.QueueNotificationFor("sess-obs", "background-task-completed", body)
	if len(got) != 1 || got[0] != (QueuedInput{Text: body, Tag: "background-task-completed", Session: "sess-obs"}) {
		t.Fatalf("observer items = %+v, want exactly the queued item", got)
	}
	if n := a.QueuedInputCountFor("sess-obs"); n != 1 {
		t.Fatalf("QueuedInputCountFor = %d, want 1", n)
	}

	// nil uninstalls the observer: the item still queues, the callback stays
	// silent.
	a.SetNotificationObserver(nil)
	a.QueueNotificationFor("sess-obs", "background-task-completed", "second body")
	if len(got) != 1 {
		t.Fatalf("observer fired after unset: %+v", got)
	}
	if n := a.QueuedInputCountFor("sess-obs"); n != 2 {
		t.Fatalf("QueuedInputCountFor = %d, want 2", n)
	}

	ctx := tracing.WithRun(t.Context(), "run-obs", "sess-obs")
	out, err := a.Invoke(ctx, []messages.Message{messages.Human("next turn")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	notif, second, human := -1, -1, -1
	for i, m := range out {
		if m.Role != messages.RoleHuman {
			continue
		}
		switch {
		case strings.Contains(m.Content, "observer-99"), strings.Contains(m.Content, "second body"):
			if strings.Count(m.Content, "<background-task-completed>") != 1 ||
				strings.Count(m.Content, "</background-task-completed>") != 1 {
				t.Fatalf("notification must carry exactly one wrapper, got: %q", m.Content)
			}
			if strings.Contains(m.Content, "observer-99") {
				notif = i
			} else {
				second = i
			}
		case m.Content == "next turn":
			human = i
		}
	}
	if notif == -1 || second == -1 || human == -1 {
		t.Fatalf("transcript missing pieces: notif=%d second=%d human=%d", notif, second, human)
	}
	if notif > human || second > human {
		t.Fatalf("notifications (%d, %d) delivered after the fresh user turn (%d), want run-start prepend ahead of it", notif, second, human)
	}
	if n := a.QueuedInputCountFor("sess-obs"); n != 0 {
		t.Fatalf("queue not drained by the run: %d left", n)
	}
}

// TestCoalesceQueuedInputs verifies the pure coalescing helper: several
// queued items collapse into ONE Human message of per-item tagged blocks —
// the same rendering formatQueuedInput uses — joined by a blank line, with
// an empty Tag falling back to the queued-user-input tag.
func TestCoalesceQueuedInputs(t *testing.T) {
	msg := CoalesceQueuedInputs([]QueuedInput{
		{Text: "shell 1 finished", Tag: "background-task-completed"},
		{Text: "  typed while away  ", Session: "s1"},
	})
	if msg.Role != messages.RoleHuman {
		t.Fatalf("role = %q, want Human", msg.Role)
	}
	want := "<background-task-completed>\nshell 1 finished\n</background-task-completed>\n\n" +
		"<queued-user-input>\ntyped while away\n</queued-user-input>"
	if msg.Content != want {
		t.Fatalf("coalesced content = %q, want %q", msg.Content, want)
	}
}
