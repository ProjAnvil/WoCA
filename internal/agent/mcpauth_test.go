package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func TestMcpAuthTool(t *testing.T) {
	authorized := ""
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		McpAuth: func(ctx context.Context, server string) error {
			authorized = server
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := a.toolByName["McpAuth"]
	if !ok {
		t.Fatal("McpAuth tool not registered when flow wired")
	}
	res, err := tool.Invoke(t.Context(), map[string]any{"server": "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if authorized != "acme" || !strings.Contains(res.Content, "Authorized MCP server acme") {
		t.Fatalf("result = %q authorized = %q", res.Content, authorized)
	}
	res, _ = tool.Invoke(t.Context(), map[string]any{})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("missing server = %q", res.Content)
	}

	// Without wiring the tool is omitted.
	bare, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bare.toolByName["McpAuth"]; ok {
		t.Fatal("McpAuth should be omitted without a wired flow")
	}
}
