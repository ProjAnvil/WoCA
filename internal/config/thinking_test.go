package config

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

// captureAnthropic spins up a fake Anthropic endpoint returning a canned
// message and records request bodies.
func captureAnthropic(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{
			"id":"msg_t","type":"message","role":"assistant","model":"claude-test",
			"stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func invokeOnce(t *testing.T, cfg Config) *[]map[string]any {
	t.Helper()
	srv, bodies := captureAnthropic(t)
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	m, err := BuildModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Invoke(context.Background(), []messages.Message{messages.Human("hi")}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return bodies
}

func TestBuildModelSendsThinkingBudget(t *testing.T) {
	bodies := invokeOnce(t, Config{Provider: ProviderAnthropic, Model: "claude-test", ThinkingBudget: 2048, MaxTokens: 8192})
	if len(*bodies) != 1 {
		t.Fatalf("requests = %d", len(*bodies))
	}
	body := (*bodies)[0]
	thinking, _ := body["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" || thinking["budget_tokens"].(float64) != 2048 {
		t.Fatalf("thinking field = %v", body["thinking"])
	}
	if body["max_tokens"].(float64) != 8192 {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
}

func TestBuildModelThinkingRaisesTightCap(t *testing.T) {
	// An output cap at or below the budget is invalid for the API; BuildModel
	// must raise it (the escalation-path linkage).
	bodies := invokeOnce(t, Config{Provider: ProviderAnthropic, Model: "claude-test", ThinkingBudget: 4096, MaxTokens: 4000})
	if got := (*bodies)[0]["max_tokens"].(float64); int(got) != 4096+ThinkingOutputHeadroom {
		t.Fatalf("max_tokens = %v, want %d", got, 4096+ThinkingOutputHeadroom)
	}
}

func TestBuildModelNoThinkingByDefault(t *testing.T) {
	bodies := invokeOnce(t, Config{Provider: ProviderAnthropic, Model: "claude-test", MaxTokens: 1000})
	if _, present := (*bodies)[0]["thinking"]; present {
		t.Fatal("thinking must be absent without a budget")
	}
}

func TestNormalizeThinkingBudget(t *testing.T) {
	n, err := Config{ThinkingBudget: 100, MaxTokens: 8192}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if n.ThinkingBudget != MinThinkingBudget {
		t.Fatalf("budget = %d, want clamp to %d", n.ThinkingBudget, MinThinkingBudget)
	}
	n, _ = Config{ThinkingBudget: 20000, MaxTokens: 8192}.Normalize()
	if n.MaxTokens != 20000+ThinkingOutputHeadroom {
		t.Fatalf("max_tokens = %d, want budget + headroom", n.MaxTokens)
	}
	n, _ = Config{ThinkingBudget: 2048, MaxTokens: 16384}.Normalize()
	if n.MaxTokens != 16384 {
		t.Fatalf("ample cap should not change: %d", n.MaxTokens)
	}
}

func TestBuildModelOpenAIIgnoresThinking(t *testing.T) {
	// The openai partner has no thinking knob: building must still succeed.
	m, err := BuildModel(Config{Provider: ProviderOpenAI, Model: "gpt-4o", ThinkingBudget: 4096})
	if err != nil || m == nil {
		t.Fatalf("openai build: %v %v", m, err)
	}
}

func TestEscalatedCapKeepsThinking(t *testing.T) {
	// The recovery path rebuilds at the escalated cap with thinking on; the
	// budget clamps to MaxThinkingBudget, which the 64k cap comfortably
	// exceeds, and the request carries both.
	bodies := invokeOnce(t, Config{Provider: ProviderAnthropic, Model: "claude-test", ThinkingBudget: 60000, MaxTokens: 64000})
	body := (*bodies)[0]
	if got := body["max_tokens"].(float64); int(got) != 64000 {
		t.Fatalf("escalated max_tokens = %v", got)
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking == nil || int(thinking["budget_tokens"].(float64)) != MaxThinkingBudget {
		t.Fatalf("thinking = %v", thinking)
	}
}
