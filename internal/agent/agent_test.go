package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
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

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("read the file")}, "t")
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

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("write a file")}, "t")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(PermissionRequest)
	if !ok {
		t.Fatalf("interrupt value = %T (%v)", ie.Value, ie.Value)
	}
	if req.ToolName != "Write" {
		t.Fatalf("interrupt tool = %q, want Write", req.ToolName)
	}

	out, err := a.Resume(t.Context(), "t", "y")
	if err != nil {
		t.Fatal(err)
	}
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write a file")}, "t")
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

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("plan a feature")}, "t")
	ie := interruptFromError(t, err)
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

	state, err := a.Resume(t.Context(), "t", "y")
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanMode() {
		t.Fatal("should have exited plan mode after approval")
	}
	if got := state[len(state)-1].Content; got != "implementing now" {
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

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("help")}, "t")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(AskUserRequest)
	if !ok {
		t.Fatalf("interrupt value = %T", ie.Value)
	}
	if len(req.Questions) != 1 || req.Questions[0].Question != "which one?" {
		t.Fatalf("questions = %+v", req.Questions)
	}

	msgs, err := a.Resume(t.Context(), "t", AskUserAnswer{Answers: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
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

	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("delegate")}, "t")
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write")}, "t")
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("rm")}, "t")
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("delegate")}, "t")
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

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("write")}, "t")
	_ = interruptFromError(t, err)

	out, err := a.ResumeCheckpoint(t.Context(), "t", "y")
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("rm")}, "t")
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

// recordingModel records the input of every model call (the second-round input
// contains the tool results produced by toolsNode).
type recordingModel struct {
	*language.FakeChatModel
	mu   sync.Mutex
	seen [][]messages.Message
}

func (c *recordingModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	c.mu.Lock()
	c.seen = append(c.seen, in)
	c.mu.Unlock()
	return c.FakeChatModel.Stream(ctx, in, opts...)
}

// BindTools returns itself: the embedded FakeChatModel.BindTools returns a bare
// copy, so the recorder would be dropped when the agent rebinds tools in New.
func (c *recordingModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return c, nil }

func TestToolPanicBecomesErrorResult(t *testing.T) {
	panicker, err := coretools.FromFunc("Panicker", "always panics",
		func(ctx context.Context, a struct{}) (coretools.Result, error) {
			panic("boom")
		})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(panicker)
	fake := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "Panicker", Args: map[string]any{}},
			}},
			messages.AI("recovered"),
		),
	)
	model := &recordingModel{FakeChatModel: fake}
	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	// pump mode: see agentapi_test.go
	for _, err := range a.Stream(t.Context(), []messages.Message{messages.Human("go")}, "t") {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
	}

	// Reaching here with the process alive proves the recover works; then
	// assert the second-round model input carries the tool result as a
	// structured error.
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.seen) < 2 {
		t.Fatalf("model called %d times, want >= 2", len(model.seen))
	}
	found := false
	for _, m := range model.seen[1] {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, `tool "Panicker" panicked: boom`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("want structured panic tool result in round-2 input: %+v", model.seen[1])
	}
}

// TestAgentTwoAsksInOneBatchConverge pins the multi-approval livelock fix:
// one AI message carrying two permission-gated calls must ask each call
// exactly once, and each call's execution decision must match the answer the
// user gave for THAT call (the fork's consumed-resume-prefix semantics; the
// old in-house engine misrouted the second answer onto the first gate and
// re-prompted forever).
func TestAgentTwoAsksInOneBatchConverge(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "x"}},
				{ID: "2", Name: "Write", Args: map[string]any{"file_path": "y.txt", "content": "y"}},
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

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("write two files")}, "t")
	asked := map[string]int{}
	for range 4 {
		ie := interruptFromError(t, err)
		req := ie.Value.(PermissionRequest)
		file, _ := req.ToolArgs["file_path"].(string)
		asked[file]++
		if asked[file] > 1 {
			t.Fatalf("asked about %s twice — livelock", file)
		}
		var out []messages.Message
		out, err = a.Resume(t.Context(), "t", file == "x.txt") // approve x, deny y
		if err == nil {
			if got := out[len(out)-1].Content; got != "done" {
				t.Fatalf("final = %q, want done", got)
			}
			for _, m := range out {
				if m.Role != messages.RoleTool {
					continue
				}
				switch m.ToolCallID {
				case "1":
					if strings.Contains(m.Content, "permission denied") {
						t.Fatalf("x.txt approved by user but denied: %q", m.Content)
					}
				case "2":
					if !strings.Contains(m.Content, "permission denied") {
						t.Fatalf("y.txt denied by user but ran: %q", m.Content)
					}
				}
			}
			return
		}
	}
	t.Fatalf("did not converge within 4 rounds (asked: %v)", asked)
}
