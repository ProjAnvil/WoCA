package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

func TestAgentToolLoop(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Read", Args: map[string]any{"file_path": "agent.go"}},
			}},
			messages.AI("All done."),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry("."),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("read the file")})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Role != messages.RoleSystem {
		t.Fatalf("first message should be system, got %v", out[0].Role)
	}
	last := out[len(out)-1]
	if last.Role != messages.RoleAI || last.Content != "All done." {
		t.Fatalf("last message = %+v", last)
	}

	foundTool := false
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == "1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatal("expected a tool result message")
	}
}

func TestAgentPermissionInterrupt(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("ok"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("write a file")})
	ie, ok := graph.AsInterrupt(err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	req, ok := ie.Value.(PermissionRequest)
	if !ok {
		t.Fatalf("interrupt value = %T (%v)", ie.Value, ie.Value)
	}
	if req.ToolName != "Write" {
		t.Fatalf("interrupt tool = %q, want Write", req.ToolName)
	}

	state, err := a.Graph().Resume(context.Background(), ie, "y")
	if err != nil {
		t.Fatal(err)
	}
	out := MessagesFrom(state)
	if out[len(out)-1].Content != "ok" {
		t.Fatalf("resumed run final = %+v", out[len(out)-1])
	}
}

func TestAgentAcceptEdits(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("wrote it"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeAcceptEdits,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("write a file")})
	if err != nil {
		t.Fatalf("acceptEdits should not interrupt on Write: %v", err)
	}
	if got := out[len(out)-1].Content; got != "wrote it" {
		t.Fatalf("final = %q", got)
	}
}

func TestAgentPlanMode(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "EnterPlanMode", Args: map[string]any{}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "2", Name: "ExitPlanMode", Args: map[string]any{"plan": "my plan"}},
			}},
			messages.AI("implementing now"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("plan a feature")})
	ie, ok := graph.AsInterrupt(err)
	if !ok {
		t.Fatalf("expected ExitPlanMode interrupt, got %v", err)
	}
	req, ok := ie.Value.(PlanApprovalRequest)
	if !ok {
		t.Fatalf("interrupt value = %T", ie.Value)
	}
	if req.Plan != "my plan" {
		t.Fatalf("plan = %q", req.Plan)
	}
	if !a.PlanMode() {
		t.Fatal("should still be in plan mode before approval")
	}

	state, err := a.Graph().Resume(context.Background(), ie, "y")
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanMode() {
		t.Fatal("should have exited plan mode after approval")
	}
	if got := MessagesFrom(state)[len(MessagesFrom(state))-1].Content; got != "implementing now" {
		t.Fatalf("final = %q", got)
	}
}

func TestAgentAskUserQuestion(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "AskUserQuestion", Args: map[string]any{
					"questions": []map[string]any{{
						"question": "which one?",
						"options":  []map[string]any{{"label": "a"}, {"label": "b"}},
					}},
				}},
			}},
			messages.AI("thanks"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("help")})
	ie, ok := graph.AsInterrupt(err)
	if !ok {
		t.Fatalf("expected AskUserQuestion interrupt, got %v", err)
	}
	req, ok := ie.Value.(AskUserRequest)
	if !ok {
		t.Fatalf("interrupt value = %T", ie.Value)
	}
	if len(req.Questions) != 1 || req.Questions[0].Question != "which one?" {
		t.Fatalf("questions = %+v", req.Questions)
	}

	state, err := a.Graph().Resume(context.Background(), ie, AskUserAnswer{Answers: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := MessagesFrom(state)
	if got := msgs[len(msgs)-1].Content; got != "thanks" {
		t.Fatalf("final = %q", got)
	}
}

func TestAgentTaskSubagent(t *testing.T) {
	subagentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("subagent report: done")),
	)
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Task", Args: map[string]any{"description": "do a thing", "prompt": "do it"}},
			}},
			messages.AI("task complete"),
		),
	)
	a, err := New(Config{
		Model:          model,
		SubagentModel:  subagentModel,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("delegate")})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[len(out)-1].Content; got != "task complete" {
		t.Fatalf("final = %q", got)
	}

	// The Task tool result should contain the subagent's report.
	foundReport := false
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "subagent report: done") {
			foundReport = true
		}
	}
	if !foundReport {
		t.Fatalf("expected subagent report in tool result; got messages: %+v", out)
	}
}

func TestAgentPreToolUseHookDenies(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "rm -rf /"}},
			}},
			messages.AI("done"),
		),
	)
	hr := hooks.NewRunner(
		hooks.Config{PreToolUse: []hooks.Entry{{Matcher: "Bash", Hooks: []hooks.Hook{{Command: "deny"}}}}},
		"/tmp", "s1",
		hooks.WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
			return hooks.CommandResult{Stdout: "blocked by hook", ExitCode: 2}, nil
		}),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Hooks:          hr,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("run it")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "blocked by hook") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected hook denial in tool result: %+v", out)
	}
}

func TestAgentPermissionRulesAllow(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("wrote"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"Write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("write")})
	if err != nil {
		t.Fatalf("allow rule should bypass the default ask: %v", err)
	}
	if got := out[len(out)-1].Content; got != "wrote" {
		t.Fatalf("final = %q", got)
	}
}

func TestAgentPermissionRulesDeny(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "rm -rf /"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Permissions:    &permissions.Rules{Deny: []string{"Bash"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("rm")})
	if err != nil {
		t.Fatalf("deny rule should not interrupt: %v", err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected denied tool result: %+v", out)
	}
}

func TestAgentTaskUnknownSubagentType(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Task", Args: map[string]any{"description": "x", "subagent_type": "nope"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("delegate")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, `unknown subagent_type "nope"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown subagent_type error: %+v", out)
	}
}

func TestAgentResumeCheckpointPersists(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("ok"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		CheckpointDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(context.Background(), []messages.Message{messages.Human("write")})
	ie, ok := graph.AsInterrupt(err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	if ie.Key == "" {
		t.Fatal("expected a persisted checkpoint key")
	}

	out, err := a.ResumeCheckpoint(context.Background(), ie.Key, "y")
	if err != nil {
		t.Fatal(err)
	}
	if got := out[len(out)-1].Content; got != "ok" {
		t.Fatalf("resumed final = %q", got)
	}
}

func TestAgentBashPolicyDeny(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "rm -rf /tmp/x"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		BashPolicy:     tools.NewCommandPolicy(nil, []string{"rm -rf"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("rm")})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "blocked by denylist") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected blocked-by-denylist tool result: %+v", out)
	}
}
