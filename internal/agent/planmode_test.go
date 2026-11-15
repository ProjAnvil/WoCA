package agent

// Enforced plan mode (spec §3.3): plan=true restricts the session to the
// read-only/CatMeta arm (the ModeReadOnly arm) whatever the base mode — a
// write is DENIED without asking, never silently run and never asked — until
// the session's ExitPlanMode is approved. These tests pin the full matrix:
// Write and write-class Bash denied in plan, classifier-read-only Bash
// allowed, the ExitPlanMode approval flip, enforced plan beating session
// grants (Task 3 review ruling #1), and the Task-spawned subagent resolving
// the PARENT session's policy entry (ruling #4).

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// planWriteModel scripts the full plan arc: enter plan mode, a Write that
// must be denied while plan is on, the ExitPlanMode approval ask, and the
// same Write again once the plan is approved.
func planWriteModel(target string) *language.FakeChatModel {
	return language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "p1", Name: "EnterPlanMode", Args: map[string]any{}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": target, "content": "planned"}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "x1", Name: "ExitPlanMode", Args: map[string]any{"plan": "my plan"}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w2", Name: "Write", Args: map[string]any{"file_path": target, "content": "planned"}},
			}},
			messages.AI("done"),
		),
	)
}

// TestPlanModeBlocksWrites pins the core §3.3 matrix end to end over a base
// acceptEdits mode (so the only thing blocking the post-approval Write would
// have been a stuck plan flag): while plan is on the Write is denied WITHOUT
// asking (the run's single pause is the ExitPlanMode approval itself), and
// after the user approves the plan the same Write proceeds.
func TestPlanModeBlocksWrites(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "plan-write.txt")
	a, err := New(Config{
		Model:          planWriteModel(target),
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeAcceptEdits,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("plan then write")}, "plan-writes")
	ie := interruptFromError(t, err)
	// The run's ONLY pause is the plan approval: the in-plan Write must have
	// been denied outright, not asked (an ask would have surfaced as a
	// PermissionRequest here instead).
	if _, ok := ie.Value.(PlanApprovalRequest); !ok {
		t.Fatalf("pause = %#v, want PlanApprovalRequest (the in-plan Write must not ask)", ie.Value)
	}
	if !a.PlanMode() {
		t.Fatal("plan flag must stay on until the user approves the exit")
	}

	state, err := a.Resume(t.Context(), "plan-writes", "y")
	if err != nil {
		t.Fatalf("resume after approval: %v", err)
	}
	if a.PlanMode() {
		t.Fatal("approved exit did not flip plan off")
	}
	// The denial of the in-plan Write is in the transcript...
	if !toolContentContains(state, "permission denied") {
		t.Error("PLAN GAP: the Write inside plan mode produced no denial marker in the transcript")
	}
	// ...and the approved-plan Write really ran.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("PLAN GAP: post-approval Write did not run: %v", err)
	}
	if got := state[len(state)-1].Content; got != "done" {
		t.Fatalf("final message = %q, want %q", got, "done")
	}
}

// TestPlanModeBashMatrix pins the Bash half of the matrix: a write-class
// command is denied without asking while plan is on, while a
// classifier-read-only command still runs.
func TestPlanModeBashMatrix(t *testing.T) {
	run := func(t *testing.T, command string) ([]messages.Message, error) {
		t.Helper()
		workdir := t.TempDir()
		model := language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(
				messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "p1", Name: "EnterPlanMode", Args: map[string]any{}},
				}},
				messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
					{ID: "b1", Name: "Bash", Args: map[string]any{"command": command}},
				}},
				messages.AI("done"),
			),
		)
		a, err := New(Config{
			Model:          model,
			Registry:       tools.NewRegistry(workdir),
			SystemPrompt:   "t",
			PermissionMode: config.ModeDefault,
			WorkDir:        workdir,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "plan-bash")
	}

	t.Run("write-class command denied without asking", func(t *testing.T) {
		workdir := t.TempDir() // only for the marker path check below
		marker := filepath.Join(workdir, "plan-bash.txt")
		state, err := run(t, "touch "+marker)
		if err != nil {
			t.Fatalf("write-class Bash in plan must be denied, not pause or fail: %v", err)
		}
		if !toolContentContains(state, "permission denied") {
			t.Error("PLAN GAP: write-class Bash inside plan mode produced no denial marker")
		}
		if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("PLAN GAP: write-class Bash executed inside plan mode (stat err = %v)", err)
		}
	})

	t.Run("classifier-read-only command allowed", func(t *testing.T) {
		state, err := run(t, "pwd")
		if err != nil {
			t.Fatalf("read-only Bash in plan must run without asking: %v", err)
		}
		if toolContentContains(state, "permission denied") {
			t.Error("PLAN GAP: classifier-read-only Bash was denied inside plan mode")
		}
		if got := state[len(state)-1].Content; got != "done" {
			t.Fatalf("final message = %q, want %q", got, "done")
		}
	})
}

// TestPlanModeBeatsSessionGrant pins Task 3 review ruling #1: enforced plan
// mode outranks session grants. A pre-existing grant (bash:git push*) must
// not punch through plan=true — the call is denied without asking — while
// the same granted call runs once ExitPlanMode's approval has flipped the
// plan flag off through the session's policy source.
func TestPlanModeBeatsSessionGrant(t *testing.T) {
	src := NewPerSessionPolicySource(config.ModeDefault)
	a := &Agent{mode: config.ModeDefault, perms: nil}
	a.policy = src
	ctx := WithSessionPolicy(t.Context(), "plan-grant", src)
	call := messages.ToolCall{ID: "pg1", Name: "Bash", Args: map[string]any{"command": "git push origin main"}}
	src.AddGrant("plan-grant", GrantKey(call)) // bash:git push*
	src.SetPlan("plan-grant", true)

	if ok, err := a.checkPermission(ctx, call); err != nil || ok {
		t.Fatalf("PLAN LEAK: granted call under plan=true = %v/%v, want denied without asking", ok, err)
	}

	// ExitPlanMode approval writes plan=false through the same source
	// (setPlanModeFor -> src.SetPlan); the grant then lifts the mode-arm ask.
	src.SetPlan("plan-grant", false)
	if ok, err := a.checkPermission(ctx, call); err != nil || !ok {
		t.Fatalf("granted call after plan approval = %v/%v, want allowed", ok, err)
	}
}

// TestSubagentResolvesParentSessionPlanPolicy pins the propagation owed from
// Task 2 (review ruling #4): a Task-spawned subagent resolves the PARENT
// session's policy entry. Session A has plan=true, so the subagent's Write
// inside A is restricted (denied — plan never asks); session B (plan=false,
// acceptEdits) runs the same Write to completion.
func TestSubagentResolvesParentSessionPlanPolicy(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "sub-write.txt")
	taskTurn := func(id string) messages.Message {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: id, Name: "Task", Args: map[string]any{
				"description": "write files",
				"prompt":      "write the files I asked for",
			}},
		}}
	}
	parentModel := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			taskTurn("t1"), messages.AI("A done"),
			taskTurn("t2"), messages.AI("B done"),
		),
	)
	src := NewPerSessionPolicySource(config.ModeDefault)
	a, err := New(Config{
		Model:          parentModel,
		SubagentModel:  &transcriptModel{script: []messages.Message{writeCall("w1", target, "x"), messages.AI("sub done")}},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		PolicySource:   src,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Session A: plan=true. The Task call itself is CatMeta (allowed); the
	// subagent's Write inside A must be restricted by A's plan entry.
	src.SetPlan("sess-a", true)
	ctxA := WithSessionPolicy(tracing.WithRun(t.Context(), "run-plan-a", "sess-a"), "sess-a", src)
	intrs, _ := pumpStream(t, a.Stream(ctxA, []messages.Message{messages.Human("do it in A")}, "thread-plan-a"))
	a.EndRun(ctxA)
	if len(intrs) != 0 {
		t.Fatalf("session A pauses = %d, want 0 (plan denies the subagent Write without asking): %v", len(intrs), intrs)
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("CHILD POLICY GAP: the subagent's Write inside plan-mode session A executed (stat err = %v)", err)
	}

	// Session B: plan=false with acceptEdits runs the same subagent Write.
	src.SetMode("sess-b", config.ModeAcceptEdits)
	ctxB := WithSessionPolicy(tracing.WithRun(t.Context(), "run-plan-b", "sess-b"), "sess-b", src)
	intrs, _ = pumpStream(t, a.Stream(ctxB, []messages.Message{messages.Human("do it in B")}, "thread-plan-b"))
	a.EndRun(ctxB)
	if len(intrs) != 0 {
		t.Fatalf("session B pauses = %d, want 0 (acceptEdits allows the Write): %v", len(intrs), intrs)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("session B's subagent Write did not run: %v", err)
	}
}
