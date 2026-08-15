package agent

import (
	"context"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// TestUsageAccumulatesAcrossCalls verifies the /cost backing store: every
// model call folds its UsageMetadata into session totals.
func TestUsageAccumulatesAcrossCalls(t *testing.T) {
	resp1 := messages.AI("first")
	resp1.UsageMetadata = messages.UsageMetadata{
		InputTokens: 100, OutputTokens: 20, TotalTokens: 120,
		InputTokenDetails: &messages.InputTokenDetails{CacheReadInputTokens: 50, CacheCreationInputTokens: 10},
	}
	resp2 := messages.AI("second")
	resp2.UsageMetadata = messages.UsageMetadata{InputTokens: 30, OutputTokens: 5, TotalTokens: 35}
	model := language.NewFakeChatModel(language.WithResponses(resp1, resp2))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(context.Background(), []messages.Message{messages.Human("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(context.Background(), []messages.Message{messages.Human("b")}); err != nil {
		t.Fatal(err)
	}
	u := a.Usage()
	if u.Requests != 2 || u.InputTokens != 130 || u.OutputTokens != 25 || u.TotalTokens != 155 {
		t.Fatalf("totals = %+v", u)
	}
	if u.CacheReadTokens != 50 || u.CacheWriteTokens != 10 {
		t.Fatalf("cache detail lost: %+v", u)
	}
}

// TestUsageZeroWhenProviderOmits ensures zero-usage responses are not counted
// as requests.
func TestUsageZeroWhenProviderOmits(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("no usage reported")))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(context.Background(), []messages.Message{messages.Human("hi")}); err != nil {
		t.Fatal(err)
	}
	if u := a.Usage(); u.Requests != 0 {
		t.Fatalf("zero-usage response counted: %+v", u)
	}
}
