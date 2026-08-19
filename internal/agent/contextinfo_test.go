package agent

import (
	"context"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func newBreakdownAgent(t *testing.T) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "AskUserQuestion", Args: map[string]any{
				"questions": []any{map[string]any{"question": "pick"}},
			}},
		}}),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "You are a test agent with a decently long prompt so the system category is non-trivial.",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestContextBreakdownCategories verifies the /context payload: categories
// cover system/memory/tools/history, the total is their sum, and history is
// split by role.
func TestContextBreakdownCategories(t *testing.T) {
	a := newBreakdownAgent(t)
	ai := messages.AI("here is a tool call")
	ai.ToolCalls = []messages.ToolCall{{ID: "t1", Name: "Read", Args: map[string]any{"path": "a.go"}}}
	history := []messages.Message{
		messages.Human("please read the file"),
		ai,
		messages.Tool("t1", "file contents here"),
	}
	info := a.ContextBreakdown(context.Background(), history)

	want := map[string]bool{"system": false, "memory": false, "tools": false, "history_human": false, "history_ai": false, "history_tool": false}
	sum := 0
	for _, c := range info.Categories {
		if _, ok := want[c.Key]; ok {
			want[c.Key] = true
		}
		sum += c.Tokens
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("category %q missing: %+v", k, info.Categories)
		}
	}
	if sum != info.EstimatedTotal {
		t.Fatalf("estimated total %d != sum of categories %d", info.EstimatedTotal, sum)
	}
	if info.EstimatedTotal == 0 {
		t.Fatalf("breakdown is all zeros: %+v", info)
	}
	byKey := map[string]ContextCategory{}
	for _, c := range info.Categories {
		byKey[c.Key] = c
	}
	if byKey["system"].Tokens == 0 {
		t.Fatalf("system category empty (prompt not counted): %+v", byKey["system"])
	}
	if byKey["tools"].Count == 0 || byKey["tools"].Tokens == 0 {
		t.Fatalf("tool schemas not counted: %+v", byKey["tools"])
	}
	if byKey["history_human"].Count != 1 || byKey["history_ai"].Count != 1 || byKey["history_tool"].Count != 1 {
		t.Fatalf("history role counts wrong: %+v", info.Categories)
	}
	// Before any model call there is no provider-reported last request.
	if info.LastRequest != nil {
		t.Fatalf("last request should be nil before the first model call: %+v", info.LastRequest)
	}
}

// TestContextBreakdownCompactedHistory verifies the compact-summary category:
// auto-compact reinserts its summary as a System message mid-history, which
// must be counted (its own line), not silently dropped.
func TestContextBreakdownCompactedHistory(t *testing.T) {
	a := newBreakdownAgent(t)
	history := []messages.Message{
		messages.System("<summary of earlier conversation>\nwe discussed the parser refactor"),
		messages.Human("continue"),
		messages.AI("done"),
	}
	info := a.ContextBreakdown(context.Background(), history)
	var sum *ContextCategory
	for i, c := range info.Categories {
		if c.Key == "history_summary" {
			sum = &info.Categories[i]
		}
	}
	if sum == nil {
		t.Fatalf("history_summary category missing: %+v", info.Categories)
	}
	if sum.Tokens == 0 || sum.Count != 1 {
		t.Fatalf("summary category = %+v", sum)
	}
	// The summary must be part of the total.
	total := 0
	for _, c := range info.Categories {
		total += c.Tokens
	}
	if total != info.EstimatedTotal {
		t.Fatalf("total %d != sum %d", info.EstimatedTotal, total)
	}
}

// TestContextBreakdownLastRequest verifies that after one model call the
// provider-reported usage of that call rides along with the breakdown.
func TestContextBreakdownLastRequest(t *testing.T) {
	resp := messages.AI("answer")
	resp.UsageMetadata = messages.UsageMetadata{
		InputTokens: 42, OutputTokens: 7, TotalTokens: 49,
		InputTokenDetails: &messages.InputTokenDetails{CacheReadInputTokens: 100},
	}
	model := language.NewFakeChatModel(language.WithResponses(resp))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	hist, err := a.Invoke(context.Background(), []messages.Message{messages.Human("hi")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	info := a.ContextBreakdown(context.Background(), hist)
	if info.LastRequest == nil {
		t.Fatalf("last request missing after a model call")
	}
	if info.LastRequest.InputTokens != 42 || info.LastRequest.CacheReadTokens != 100 || info.LastRequest.OutputTokens != 7 {
		t.Fatalf("last request = %+v", info.LastRequest)
	}
}
