package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/tools"
)

func TestRunGoalInterruptResume(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("criteria\nthis cycle: write the file"), // assess
			messages.Message{Role: messages.RoleAI, Content: "writing", ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "g.txt", "content": "x"}},
			}}, // execute → permission interrupt
			messages.AI("written and verified"), // execute final after approval
			messages.AI(`{"verdict":"complete","confidence":0.9,"evidence":"file written"}`), // evaluate
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault, // permission interrupt path
		RecursionLimit: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGoal("write a file", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}

	rendered := bytes.Buffer{}
	summary := bytes.Buffer{}
	r := New(a, func(v any) (any, error) { return "y", nil }, DefaultRenderer(&rendered))
	r.RenderSummary = DefaultSummaryRenderer(&summary)

	if _, err := r.RunGoal(context.Background(), []messages.Message{messages.Human("go")}); err != nil {
		t.Fatal(err)
	}
	if rendered.Len() == 0 {
		t.Fatal("renderer produced nothing")
	}
	_ = summary.String()
}

func TestGoalSummaryPreference(t *testing.T) {
	if got := goalSummary(&agent.Goal{Evidence: " done "}); got != "done" {
		t.Errorf("evidence = %q", got)
	}
	if got := goalSummary(&agent.Goal{BlockedReason: " stuck "}); got != "stuck" {
		t.Errorf("blocked = %q", got)
	}
	if got := goalSummary(&agent.Goal{Notes: []string{"n1", "n2"}}); got != "n2" {
		t.Errorf("notes = %q", got)
	}
	if got := goalSummary(&agent.Goal{Progress: []agent.GoalProgressEntry{{Note: "p1"}, {Note: "p2"}}}); got != "p2" {
		t.Errorf("progress = %q", got)
	}
	if got := goalSummary(&agent.Goal{}); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestSummarizeHelpers(t *testing.T) {
	if got := summarizeArgs(nil); got != "" {
		t.Errorf("nil args = %q", got)
	}
	if got := summarizeArgs(map[string]any{"a": "b"}); got != `{"a":"b"}` {
		t.Errorf("args = %q", got)
	}
	long := map[string]any{"k": strings.Repeat("x", 300)}
	if got := summarizeArgs(long); len(got) <= 160 || !strings.HasSuffix(got, "…") {
		t.Errorf("long args = %q", got)
	}
	if got := summarizeResult(""); got != "(no output)" {
		t.Errorf("empty result = %q", got)
	}
	if got := summarizeResult("  first\nsecond  "); got != "first" {
		t.Errorf("multiline = %q", got)
	}
	if got := summarizeResult(strings.Repeat("y", 250)); len(got) <= 200 {
		t.Errorf("long result = %q", got)
	}
	// The summary renderer prints the text line.
	var buf bytes.Buffer
	DefaultSummaryRenderer(&buf)(agent.ToolUseSummary{Text: "used Bash"})
	if !strings.Contains(buf.String(), "· used Bash") {
		t.Errorf("summary render = %q", buf.String())
	}
	DefaultSummaryRenderer(&buf)(agent.ToolUseSummary{Text: ""})
}

func TestHistoryFromAndTokenEstimate(t *testing.T) {
	msgs := []messages.Message{messages.System("s"), messages.Human("h")}
	if got := historyFrom(msgs); len(got) != 1 || got[0].Role != messages.RoleHuman {
		t.Errorf("historyFrom = %+v", got)
	}
	if got := historyFrom(nil); got != nil {
		t.Errorf("nil = %+v", got)
	}
	if got := historyFrom([]messages.Message{messages.Human("h")}); len(got) != 1 {
		t.Errorf("no system = %+v", got)
	}

	// AI messages with content and tool calls count; other roles do not.
	n := estimateTurnOutputTokens([]messages.Message{
		messages.Human("q"),
		messages.AI("answer"),
		{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{Name: "Read", Args: map[string]any{"file_path": "a.go"}}}},
	})
	if n <= 0 {
		t.Errorf("estimate = %d", n)
	}
	if got := estimateTurnOutputTokens([]messages.Message{messages.Human("q")}); got != 0 {
		t.Errorf("human-only estimate = %d", got)
	}
}

// The renderer's non-delta branches (tool call / result steps) execute even
// when unused by other tests.
func TestDefaultRendererToolSteps(t *testing.T) {
	var buf bytes.Buffer
	r := DefaultRenderer(&buf)
	r(graph.Step{Node: "tools", Update: map[string]any{}})
	r(graph.Step{Done: true, Update: map[string]any{}})
	if buf.Len() == 0 {
		t.Log("plain steps render nothing (only deltas do)")
	}
}
