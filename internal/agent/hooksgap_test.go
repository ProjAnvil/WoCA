package agent

import (
	"context"
	"encoding/json"
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
