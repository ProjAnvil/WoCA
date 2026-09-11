package sdk

import (
	"errors"
	"testing"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
)

func TestBuildOptionsTranslatesFields(t *testing.T) {
	o := Options{
		Model: "claude-sonnet-4-5", Provider: "anthropic", APIKey: "k",
		BaseURL: "https://x", Workdir: "/tmp/w", ExtraPrompt: "be brief",
		PermissionMode: "bypassPermissions", MaxTurns: 3, MaxTokens: 4096,
		ThinkingBudget: 1024, Temperature: 0.5,
		MCPServers: []string{"s=cmd"}, AllowCommands: []string{"git"},
		DenyCommands: []string{"rm -rf"}, DB: "none",
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
	}
	bo := o.buildOptions("do it")
	want := agentbuild.Options{
		Prompt: "do it", Model: "claude-sonnet-4-5", Provider: "anthropic",
		APIKey: "k", BaseURL: "https://x", MaxTokens: 4096, ThinkingBudget: 1024,
		Temperature: 0.5, Mode: "bypassPermissions", Workdir: "/tmp/w",
		ExtraPrompt: "be brief", LogLevel: "error", MCPServers: []string{"s=cmd"},
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
		AllowCommands: []string{"git"}, DenyCommands: []string{"rm -rf"},
		DB: "none", MaxTurns: 3,
	}
	if bo.Prompt != want.Prompt || bo.Model != want.Model || bo.Mode != want.Mode ||
		bo.DB != want.DB || bo.MaxTurns != want.MaxTurns || bo.LogLevel != want.LogLevel ||
		bo.Workdir != want.Workdir || bo.ExtraPrompt != want.ExtraPrompt ||
		bo.FallbackModel != want.FallbackModel || bo.CheckpointDir != want.CheckpointDir ||
		bo.Temperature != want.Temperature || bo.ThinkingBudget != want.ThinkingBudget ||
		len(bo.MCPServers) != 1 || len(bo.AllowCommands) != 1 || len(bo.DenyCommands) != 1 {
		t.Fatalf("translation mismatch: %+v", bo)
	}
}

func TestDefaultInterruptHandlerAutoDenies(t *testing.T) {
	h := (Options{}).interruptHandler()

	v, err := h(agent.PermissionRequest{ToolName: "Bash", Reason: "r"})
	if err != nil || v != "n" {
		t.Fatalf("permission: got %v, %v", v, err)
	}
	v, err = h(agent.AskUserRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok := v.(agent.AskUserAnswer)
	if !ok || len(ans.Answers) != 0 {
		t.Fatalf("ask-user: got %#v", v)
	}
	v, err = h(agent.PlanApprovalRequest{Plan: "p"})
	if err != nil || v != "n" {
		t.Fatalf("plan: got %v, %v", v, err)
	}
	_, err = h(struct{}{})
	if err == nil {
		t.Fatal("unknown interrupt type must error")
	}
}

func TestCustomInterruptHandlerWins(t *testing.T) {
	sentinel := errors.New("denied by user code")
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		if i.Kind != "permission" || i.Permission.ToolName != "Bash" {
			t.Fatalf("unexpected interrupt: %+v", i)
		}
		return nil, sentinel
	}}
	_, err := o.interruptHandler()(agent.PermissionRequest{ToolName: "Bash"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
}
