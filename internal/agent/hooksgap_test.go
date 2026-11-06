package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/tools"
)

// permHookRunner records PermissionRequest events and returns a canned
// decision stdout.
func permHookRunner(stdout string, seen *[]hooks.Event) hooks.CommandRunner {
	return func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		var ev hooks.Event
		_ = json.Unmarshal([]byte(stdin), &ev)
		*seen = append(*seen, ev)
		return hooks.CommandResult{Stdout: stdout}, nil
	}
}

// newPermTestAgent builds a default-mode agent that issues one Write call
// (which normally requires the permission interrupt).
func newPermTestAgent(t *testing.T, hr hooks.CommandRunner) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("written"),
		),
	)
	hookCfg := hooks.Config{
		PermissionRequest: []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Type: "command", Command: "decide"}}}},
	}
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Hooks:          hooks.NewRunner(hookCfg, ".", "s1", hooks.WithCommandRunner(hr)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPermissionRequestHookPreApproves(t *testing.T) {
	var seen []hooks.Event
	a := newPermTestAgent(t, permHookRunner("allow", &seen))
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	// Without pre-approval the Write gate would interrupt in default mode; a
	// completed run with the tool result means the hook allowed it.
	var toolResult string
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == "w1" {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "Wrote") {
		t.Fatalf("write not executed (tool result %q): %+v", toolResult, out)
	}
	if len(seen) != 1 || seen[0].ToolName != "Write" {
		t.Fatalf("hook events = %+v", seen)
	}
}

func TestPermissionRequestHookPreDenies(t *testing.T) {
	var seen []hooks.Event
	a := newPermTestAgent(t, permHookRunner("deny", &seen))
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("write it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == "w1" {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "permission denied") {
		t.Fatalf("expected denial result, got %q", toolResult)
	}
}

func TestPostSamplingHookFiresAsynchronously(t *testing.T) {
	var mu sync.Mutex
	var seen []hooks.Event
	hr := func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		var ev hooks.Event
		_ = json.Unmarshal([]byte(stdin), &ev)
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
		return hooks.CommandResult{}, nil
	}
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("sampled response")))
	hookCfg := hooks.Config{
		PostSampling: []hooks.Entry{{Hooks: []hooks.Hook{{Type: "command", Command: "analyze"}}}},
	}
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Hooks:          hooks.NewRunner(hookCfg, ".", "s1", hooks.WithCommandRunner(hr)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t"); err != nil {
		t.Fatal(err)
	}
	// The hook runs in a goroutine: poll briefly for its arrival.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			if seen[0].Message != "sampled response" {
				t.Fatalf("event message = %q", seen[0].Message)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PostSampling hook never fired")
}

func TestSessionEndHook(t *testing.T) {
	var seen []hooks.Event
	hr := func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		var ev hooks.Event
		_ = json.Unmarshal([]byte(stdin), &ev)
		seen = append(seen, ev)
		return hooks.CommandResult{}, nil
	}
	hookCfg := hooks.Config{
		SessionEnd: []hooks.Entry{{Hooks: []hooks.Hook{{Type: "command", Command: "cleanup"}}}},
	}
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Hooks:          hooks.NewRunner(hookCfg, ".", "s1", hooks.WithCommandRunner(hr)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunSessionEnd(t.Context(), "exit"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].StopReason != "exit" {
		t.Fatalf("events = %+v", seen)
	}
}

// firingLog records hook firings in order: each entry pairs the hook command
// with the decoded event, so tests can assert both sequence and payloads.
type firingLog struct {
	mu      sync.Mutex
	fired   []string
	events  []hooks.Event
	results map[string]hooks.CommandResult
}

func (l *firingLog) runner() hooks.CommandRunner {
	return func(ctx context.Context, command, stdin, cwd string) (hooks.CommandResult, error) {
		var ev hooks.Event
		_ = json.Unmarshal([]byte(stdin), &ev)
		l.mu.Lock()
		l.fired = append(l.fired, command)
		l.events = append(l.events, ev)
		l.mu.Unlock()
		if r, ok := l.results[command]; ok {
			return r, nil
		}
		return hooks.CommandResult{}, nil
	}
}

func (l *firingLog) snapshot() ([]string, []hooks.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.fired), slices.Clone(l.events)
}

// newSubagentHookAgent builds a bypass agent whose subagent model answers
// "report body", wired to the given hook configuration.
func newSubagentHookAgent(t *testing.T, hookCfg hooks.Config, hr hooks.CommandRunner) *Agent {
	t.Helper()
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("done"))),
		SubagentModel:  language.NewFakeChatModel(language.WithResponses(messages.AI("report body"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Hooks:          hooks.NewRunner(hookCfg, ".", "s1", hooks.WithCommandRunner(hr)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestForegroundTaskFiresSubagentStartThenStop: a foreground Task fires
// SubagentStart before the subagent runs and SubagentStop after its report,
// both carrying the resolved agent_name.
func TestForegroundTaskFiresSubagentStartThenStop(t *testing.T) {
	log := &firingLog{}
	a := newSubagentHookAgent(t, hooks.Config{
		SubagentStart: []hooks.Entry{{Matcher: "*", Hooks: []hooks.Hook{{Type: "command", Command: "on-start"}}}},
		SubagentStop:  []hooks.Entry{{Matcher: "*", Hooks: []hooks.Hook{{Type: "command", Command: "on-stop"}}}},
	}, log.runner())

	res, err := a.runTask(t.Context(), taskArgs{Description: "hooked work"})
	if err != nil {
		t.Fatal(err)
	}
	fired, events := log.snapshot()
	if len(fired) != 2 || fired[0] != "on-start" || fired[1] != "on-stop" {
		t.Fatalf("hook firing order = %v, want [on-start on-stop]", fired)
	}
	for i, ev := range events {
		if ev.AgentName != "general" {
			t.Fatalf("event %d agent_name = %q, want general", i, ev.AgentName)
		}
	}
	// No exit-2 feedback: the report passes through unwrapped.
	if res.Content != "report body" {
		t.Fatalf("report = %q", res.Content)
	}
}

// TestSubagentStopFeedbackAppendedToReport: a SubagentStop hook exiting 2 has
// its stdout appended to the subagent report inside the documented wrapper
// (no re-run of the subagent).
func TestSubagentStopFeedbackAppendedToReport(t *testing.T) {
	log := &firingLog{results: map[string]hooks.CommandResult{
		"on-stop": {Stdout: "verify the diff before trusting it", ExitCode: 2},
	}}
	a := newSubagentHookAgent(t, hooks.Config{
		SubagentStop: []hooks.Entry{{Matcher: "*", Hooks: []hooks.Hook{{Type: "command", Command: "on-stop"}}}},
	}, log.runner())

	res, err := a.runTask(t.Context(), taskArgs{Description: "checked work"})
	if err != nil {
		t.Fatal(err)
	}
	want := "report body\n<subagent-stop-feedback>\nverify the diff before trusting it\n</subagent-stop-feedback>"
	if res.Content != want {
		t.Fatalf("report = %q, want %q", res.Content, want)
	}
}

// TestNotificationHookFiresOnPermissionInterrupt: on the permission path a
// Notification hook observes "permission required: <tool>" immediately before
// the permission interrupt pauses the run.
func TestNotificationHookFiresOnPermissionInterrupt(t *testing.T) {
	log := &firingLog{}
	a, err := New(Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(
				messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
				}},
				messages.AI("written"),
			),
		),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Hooks: hooks.NewRunner(hooks.Config{
			Notification: []hooks.Entry{{Matcher: "*", Hooks: []hooks.Hook{{Type: "command", Command: "notify"}}}},
		}, ".", "s1", hooks.WithCommandRunner(log.runner())),
	})
	if err != nil {
		t.Fatal(err)
	}

	intrs, _ := pumpStream(t, a.Stream(t.Context(), []messages.Message{messages.Human("write it")}, "thread-notify"))
	// The run must have paused on the permission interrupt (the Notification
	// hook's fire site sits immediately in front of it).
	if len(intrs) != 1 {
		t.Fatalf("interrupts = %d, want 1", len(intrs))
	}
	if req, ok := decodeInterruptPayload(intrs[0].Value).(PermissionRequest); !ok || req.ToolName != "Write" {
		t.Fatalf("interrupt payload = %#v, want PermissionRequest for Write", DecodeInterruptValue(intrs[0].Value))
	}
	fired, events := log.snapshot()
	if len(fired) != 1 || fired[0] != "notify" {
		t.Fatalf("hook firings = %v, want [notify]", fired)
	}
	if len(events) != 1 || events[0].Message != "permission required: Write" {
		t.Fatalf("notification event = %+v", events)
	}
}
