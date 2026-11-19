package agent

// File-plane permission rules at the gate (deepagents borrow): path globs
// ride the existing rule precedence — explicit rules outrank the mode arms,
// so an allow lifts the default-mode ask for the matched paths only, a deny
// blocks even under bypass, and an ask re-asks paths an auto-accept mode
// would have accepted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

func writeFileCall(id, path string) messages.ToolCall {
	return messages.ToolCall{ID: id, Name: "Write", Args: map[string]any{
		"file_path": path, "content": "x",
	}}
}

// An allow rule on docs/** lifts the default-mode ask for docs only; a write
// outside it still interrupts.
func TestFilePlaneAllowRuleLiftsDefaultAskForMatchedPaths(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{writeFileCall("w1", "docs/a.md")}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"FileWrite(docs/**)"}},
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("go")}, "fplane-allow"); err != nil {
		t.Fatalf("allowed-path write interrupted or failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "docs", "a.md")); err != nil {
		t.Fatalf("allowed write did not run: %v", err)
	}

	outside := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{writeFileCall("w2", "src/b.go")}},
			messages.AI("done"),
		),
	)
	a2, err := New(Config{
		Model:          outside,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"FileWrite(docs/**)"}},
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a2.Invoke(t.Context(), []messages.Message{messages.Human("go")}, "fplane-ask")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(ApprovalRequest)
	if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
		t.Fatalf("outside-path write interrupt = %#v, want one Write ask", ie.Value)
	}
}

// A deny rule blocks the write even under bypass: explicit rules outrank the
// mode arms, and the denied call answers with the permission-denied error.
func TestFilePlaneDenyRuleBeatsBypass(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{writeFileCall("w1", "config/.env")}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		Permissions:    &permissions.Rules{Deny: []string{"FileWrite(**/.env)"}},
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("go")}, "fplane-deny")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "config", ".env")); !os.IsNotExist(err) {
		t.Fatalf("denied write landed on disk (%v)", err)
	}
	var denied bool
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "permission denied") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no permission-denied result in transcript: %+v", out)
	}
}

// An ask rule re-asks paths that ModeAcceptEdits would auto-accept.
func TestFilePlaneAskRuleReasksUnderAcceptEdits(t *testing.T) {
	workdir := t.TempDir()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{writeFileCall("w1", "secrets/token.txt")}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeAcceptEdits,
		Permissions:    &permissions.Rules{Ask: []string{"FileWrite(secrets/**)"}},
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("go")}, "fplane-askmode")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(ApprovalRequest)
	if !ok || len(req.Asks) != 1 {
		t.Fatalf("ask-rule write interrupt = %#v, want one ask", ie.Value)
	}
}
