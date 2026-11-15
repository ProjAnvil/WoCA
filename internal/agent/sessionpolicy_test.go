package agent

// Per-session permission policy (design §3.3): mode and plan state belong to
// one session, and the parent's rules/hooks reach into Task-spawned
// subagents (audit A10/A2). X1 is revived from the 2026-11-09 analysis
// round, adapted from the agent-global fields to an injected per-session
// source; the inheritance tests pin the subagent wiring.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

func TestSessionPolicyContextCarrier(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	ctx := WithSessionPolicy(t.Context(), "sess-1", src)
	gotSession, gotSrc, ok := SessionPolicyFrom(ctx)
	if !ok || gotSession != "sess-1" || gotSrc != SessionPolicySource(src) {
		t.Fatalf("carrier round-trip = %q/%v/%v", gotSession, gotSrc, ok)
	}
	// A nil source is a no-op decoration.
	if _, _, ok := SessionPolicyFrom(WithSessionPolicy(t.Context(), "s", nil)); ok {
		t.Fatal("nil source must not install a carrier")
	}
	if _, _, ok := SessionPolicyFrom(t.Context()); ok {
		t.Fatal("bare context must not carry a policy")
	}
}

func TestPerSessionPolicySource(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	// Unknown sessions inherit the base policy.
	if p := src.For("fresh"); p.Mode != config.ModeDefault || p.Plan {
		t.Fatalf("fresh session policy = %+v", p)
	}
	// Writes to the unnamed session update the inherited base.
	src.SetMode("", config.ModeBypass)
	if p := src.For("other"); p.Mode != config.ModeBypass {
		t.Fatalf("base write not inherited: %+v", p)
	}
	// Named-session writes are isolated per session.
	src.SetMode("a", config.ModeReadOnly)
	src.SetPlan("a", true)
	if p := src.For("a"); p.Mode != config.ModeReadOnly || !p.Plan {
		t.Fatalf("session a = %+v", p)
	}
	if p := src.For("b"); p.Mode != config.ModeBypass || p.Plan {
		t.Fatalf("session b leaked a's write: %+v", p)
	}
	// Snapshots are copies: mutating a returned policy must not stick.
	p := src.For("a")
	p.Plan = false
	if !src.For("a").Plan {
		t.Fatal("For must return a snapshot, not the live entry")
	}
}

// TestSessionPolicyPlanApprovalIsolatedPerSession revives X1: session A
// enters plan mode; session B (another session on the same agent) enters and
// exits plan mode with the user's approval; A's later ExitPlanMode must still
// ask A's user to approve A's plan. The agent-global planMode flag made B's
// approval waive A's approval; with a per-session source each session's plan
// state is its own.
func TestSessionPolicyPlanApprovalIsolatedPerSession(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			// session A, turn 1: enter plan mode
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "a1", Name: "EnterPlanMode", Args: map[string]any{}}}},
			messages.AI("A: researching"),
			// session B: enter plan mode, then ask to exit it
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "b1", Name: "EnterPlanMode", Args: map[string]any{}}}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "b2", Name: "ExitPlanMode", Args: map[string]any{"plan": "B plan"}}}},
			messages.AI("B: implementing"),
			// session A, turn 2: ask to exit plan mode
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "a2", Name: "ExitPlanMode", Args: map[string]any{"plan": "A plan"}}}},
			messages.AI("A: implementing"),
		),
	)
	src := NewPerSessionPolicySource(config.ModeDefault)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PolicySource:   src,
		WorkDir:        t.TempDir(),
		PermissionMode: config.ModeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctxA1 := WithSessionPolicy(tracing.WithRun(t.Context(), "x1-run-a1", "sess-a"), "sess-a", src)
	if intrs, _ := pumpStream(t, a.Stream(ctxA1, []messages.Message{messages.Human("plan A")}, "thread-x1-a")); len(intrs) != 0 {
		t.Fatalf("A turn 1 paused unexpectedly: %v", intrs)
	}
	a.EndRun(ctxA1)
	if !src.For("sess-a").Plan {
		t.Fatalf("A's EnterPlanMode did not set A's plan flag: %+v", src.For("sess-a"))
	}

	ctxB := WithSessionPolicy(tracing.WithRun(t.Context(), "x1-run-b", "sess-b"), "sess-b", src)
	intrs, _ := pumpStream(t, a.Stream(ctxB, []messages.Message{messages.Human("plan B")}, "thread-x1-b"))
	if len(intrs) != 1 {
		t.Fatalf("B pauses = %d, want 1 (its plan approval)", len(intrs))
	}
	if req, ok := decodeInterruptPayload(intrs[0].Value).(ApprovalRequest); !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "ExitPlanMode" {
		t.Fatalf("B pause = %T, want an ExitPlanMode ask", decodeInterruptPayload(intrs[0].Value))
	}
	pumpStream(t, a.ResumeStream(ctxB, "thread-x1-b", "y"))
	a.EndRun(ctxB)
	if src.For("sess-b").Plan {
		t.Fatalf("B's approved exit did not clear B's plan flag: %+v", src.For("sess-b"))
	}
	if !src.For("sess-a").Plan {
		t.Fatal("B's approval leaked into A's plan flag")
	}

	ctxA2 := WithSessionPolicy(tracing.WithRun(t.Context(), "x1-run-a2", "sess-a"), "sess-a", src)
	intrs, final := pumpStream(t, a.Stream(ctxA2, []messages.Message{messages.Human("exit plan A")}, "thread-x1-a"))
	logToolMsgs(t, "A-turn2", final)
	if len(intrs) != 1 {
		t.Errorf("CROSS-SESSION LEAK: A's ExitPlanMode raised %d approval prompts, want 1 — B's approval waived A's plan approval", len(intrs))
	} else {
		// Approve A's plan and finish the turn cleanly.
		if req, ok := decodeInterruptPayload(intrs[0].Value).(ApprovalRequest); !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "ExitPlanMode" {
			t.Errorf("A pause = %T, want an ExitPlanMode ask", decodeInterruptPayload(intrs[0].Value))
		}
		pumpStream(t, a.ResumeStream(ctxA2, "thread-x1-a", "y"))
		if src.For("sess-a").Plan {
			t.Error("A's approved exit did not clear A's plan flag")
		}
	}
	a.EndRun(ctxA2)
}

// inheritanceParentModel scripts the parent to one foreground Task call.
func inheritanceParentModel() *language.FakeChatModel {
	return language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Task", Args: map[string]any{
					"description": "write files",
					"prompt":      "write the files I asked for",
				}},
			}},
			messages.AI("parent done"),
		),
	)
}

// TestSubagentInheritsRulesAndHooks (audit A10/A2): a Task-spawned subagent
// runs inside the parent's trust boundary, so the parent's explicit
// permission rules and PreToolUse hooks must apply to the subagent's own
// tool calls. Today newSubagent passes neither, so a deny rule and a denying
// hook are both silently skipped inside the child.
func TestSubagentInheritsRulesAndHooks(t *testing.T) {
	t.Run("rules deny applies inside subagent", func(t *testing.T) {
		workdir := t.TempDir()
		target := filepath.Join(workdir, "denied.txt")
		a, err := New(Config{
			Model:          inheritanceParentModel(),
			SubagentModel:  &transcriptModel{script: []messages.Message{writeCall("w1", target, "x"), messages.AI("sub done")}},
			Registry:       tools.NewRegistry(workdir),
			SystemPrompt:   "t",
			PermissionMode: config.ModeBypass,
			Permissions:    &permissions.Rules{Deny: []string{"Write"}},
			WorkDir:        workdir,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, "thread-inherit-rules"))
		a.EndRun(ctx)
		if len(intrs) != 0 {
			t.Fatalf("pauses = %d, want 0 (a deny rule must not ask)", len(intrs))
		}
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RULES GAP: the subagent's Write executed despite the parent's deny rule (stat err = %v)", err)
		}
	})

	t.Run("PreToolUse hook fires for subagent tool calls", func(t *testing.T) {
		workdir := t.TempDir()
		target := filepath.Join(workdir, "hooked.txt")
		log := &firingLog{results: map[string]hooks.CommandResult{
			"deny-write": {ExitCode: 2},
		}}
		a, err := New(Config{
			Model:          inheritanceParentModel(),
			SubagentModel:  &transcriptModel{script: []messages.Message{writeCall("w1", target, "x"), messages.AI("sub done")}},
			Registry:       tools.NewRegistry(workdir),
			SystemPrompt:   "t",
			PermissionMode: config.ModeBypass,
			Hooks: hooks.NewRunner(hooks.Config{
				PreToolUse: []hooks.Entry{{Matcher: "Write", Hooks: []hooks.Hook{{Type: "command", Command: "deny-write"}}}},
			}, ".", "s1", hooks.WithCommandRunner(log.runner())),
			WorkDir: workdir,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, "thread-inherit-hooks"))
		a.EndRun(ctx)
		if len(intrs) != 0 {
			t.Fatalf("pauses = %d, want 0 (the hook denies before any ask)", len(intrs))
		}
		fired, events := log.snapshot()
		sawWrite := false
		for _, ev := range events {
			if ev.ToolName == "Write" {
				sawWrite = true
			}
		}
		if len(fired) == 0 || !sawWrite {
			t.Errorf("HOOKS GAP: PreToolUse hook never fired for the subagent's Write (firings = %v)", fired)
		}
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("HOOKS GAP: the subagent's Write executed despite the denying PreToolUse hook (stat err = %v)", err)
		}
	})
}
