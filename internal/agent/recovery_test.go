package agent

import (
	"context"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func truncatedAI(content string) messages.Message {
	m := messages.AI(content)
	m.ResponseMetadata = map[string]any{"stop_reason": "max_tokens"}
	return m
}

func TestIsMaxOutputTokensTruncated(t *testing.T) {
	if !IsMaxOutputTokensTruncated(truncatedAI("cut")) {
		t.Fatal("stop_reason=max_tokens not detected")
	}
	m := messages.AI("cut")
	m.ResponseMetadata = map[string]any{"finish_reason": "length"}
	if !IsMaxOutputTokensTruncated(m) {
		t.Fatal("finish_reason=length not detected")
	}
	if IsMaxOutputTokensTruncated(messages.AI("done")) {
		t.Fatal("normal message misdetected")
	}
	normal := messages.AI("done")
	normal.ResponseMetadata = map[string]any{"stop_reason": "end_turn"}
	if IsMaxOutputTokensTruncated(normal) {
		t.Fatal("end_turn misdetected")
	}
	if IsMaxOutputTokensTruncated(messages.Human("max_tokens")) {
		t.Fatal("human message misdetected")
	}
}

func TestInvokeOutputLimitRecovery(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			truncatedAI("part one of the answer..."),
			messages.AI("part two, complete."),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("long answer please")})
	if err != nil {
		t.Fatal(err)
	}

	// The recovery continuation message is injected between the truncated
	// response and the completed one.
	var sawRecovery, sawTruncated, sawComplete bool
	for _, m := range out {
		switch {
		case m.Role == messages.RoleHuman && m.Content == OutputLimitRecoveryPrompt:
			sawRecovery = true
		case m.Role == messages.RoleAI && m.Content == "part one of the answer...":
			sawTruncated = true
		case m.Role == messages.RoleAI && m.Content == "part two, complete.":
			sawComplete = true
		}
	}
	if !sawRecovery || !sawTruncated || !sawComplete {
		t.Fatalf("transcript missing pieces: recovery=%v truncated=%v complete=%v", sawRecovery, sawTruncated, sawComplete)
	}
	if last := out[len(out)-1]; last.Role != messages.RoleAI || last.Content != "part two, complete." {
		t.Fatalf("final message = %+v", last)
	}
	// Exactly two model turns: the truncated one and the recovered one.
	if got := len(out); got != 5 { // system, human, truncated AI, recovery, complete AI
		t.Fatalf("transcript length = %d: %+v", got, out)
	}
}

func TestInvokeOutputLimitExhaustion(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(truncatedAI("still cut")),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("long answer please")})
	if err != nil {
		t.Fatal(err)
	}

	// 1 original turn + MaxOutputTokensRecoveries continuation turns; the
	// FakeChatModel cycles its single truncated response.
	wantAI := 1 + MaxOutputTokensRecoveries
	gotAI := 0
	for _, m := range out {
		if m.Role == messages.RoleAI {
			gotAI++
		}
	}
	if gotAI != wantAI {
		t.Fatalf("AI messages = %d, want %d (original + 3 recoveries)", gotAI, wantAI)
	}
}

func TestInvokeOutputLimitEscalation(t *testing.T) {
	// The primary model always truncates; the escalated model completes.
	primary := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(truncatedAI("cut at 8k")),
	)
	escalated := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("full answer at 64k")),
	)
	escCalls := 0
	a, err := New(Config{
		Model:          primary,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		MaxTokensEscalator: func() (language.ChatModel, error) {
			escCalls++
			return escalated, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := a.Invoke(context.Background(), []messages.Message{messages.Human("long answer please")})
	if err != nil {
		t.Fatal(err)
	}

	if escCalls != 1 {
		t.Fatalf("escalator called %d times, want 1", escCalls)
	}
	// Escalation retries the same request: no recovery prompt is injected.
	for _, m := range out {
		if m.Role == messages.RoleHuman && m.Content == OutputLimitRecoveryPrompt {
			t.Fatal("escalation path must not inject a recovery prompt")
		}
	}
	if last := out[len(out)-1]; last.Content != "full answer at 64k" {
		t.Fatalf("final message = %+v", last)
	}
	// The original model is restored after the turn.
	if a.CurrentModel() != primary {
		t.Fatal("original model not restored after escalation")
	}
}

func TestTruncatedTailTolerance(t *testing.T) {
	if TruncatedTail(nil) {
		t.Fatal("nil transcript misdetected")
	}
	msgs := []messages.Message{messages.Human("q"), truncatedAI("a")}
	if !TruncatedTail(msgs) {
		t.Fatal("truncated tail not detected")
	}
	// A tool result after the AI message means the turn continued.
	msgs = append(msgs, messages.Tool("1", "ok"))
	if TruncatedTail(msgs) {
		t.Fatal("tail with tool result misdetected")
	}
}
