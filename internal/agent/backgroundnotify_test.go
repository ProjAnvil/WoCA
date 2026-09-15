package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
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
	if _, err := reg.Background().Start("echo idle-77", "", "."); err != nil {
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
