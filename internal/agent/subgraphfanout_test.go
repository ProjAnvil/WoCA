package agent

// Task 7 (graph-native HITL β core): foreground Task calls fan out from
// route2 as types.Send tasks of content-addressed subagent child subgraphs;
// a child's final AI message terminates through a parent-targeted Command
// that backfills messages.Tool(parentCallID, report) into the PARENT state.
// These tests revive the archived relay-idempotency repros N1 and R1 against
// the subgraph topology (archive:
// woca-internal-docs/relay-idempotency-repro-2026-11-08_test.go.txt) and pin
// the new fan-out mechanics (parallel same-def Sends, cap rejection,
// background invariance, shell attribution, resume pre-checks).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// promptFileModel is N1's subagent model (revived from the archive): the
// first round writes the file named by the child prompt (FILE-A → a.txt,
// FILE-B → b.txt); later rounds report done.
type promptFileModel struct{}

func (m *promptFileModel) pick(input []messages.Message) messages.Message {
	prompt := ""
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
		if msg.Role == messages.RoleHuman && prompt == "" {
			prompt = msg.Content
		}
	}
	file := "a.txt"
	if strings.Contains(prompt, "FILE-B") {
		file = "b.txt"
	}
	return writeCall("w-"+file, file, "x")
}

func (m *promptFileModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *promptFileModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		resp, _ := m.Invoke(ctx, in, opts...)
		out[i] = resp
	}
	return out, nil
}

func (m *promptFileModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *promptFileModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *promptFileModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *promptFileModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *promptFileModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// n1ParentModel scripts the N1 parent: one batch with two foreground Tasks.
// n1ParentModel is transcript-driven (a cursor-scripted FakeChatModel would
// replay the Task batch on the second round: the model node re-binds tools
// per round and each bind is a fresh cursor copy — the same reason the
// relay-era tests use transcriptModel): it re-issues the two Task calls
// until BOTH call ids have tool results, then reports done.
type n1ParentModel struct{}

func (m *n1ParentModel) pick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	if answered["tA"] && answered["tB"] {
		return messages.AI("parent done")
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "tA", Name: "Task", Args: map[string]any{"description": "a", "prompt": "FILE-A write it"}},
		{ID: "tB", Name: "Task", Args: map[string]any{"description": "b", "prompt": "FILE-B write it"}},
	}}
}

func (m *n1ParentModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *n1ParentModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *n1ParentModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *n1ParentModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *n1ParentModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *n1ParentModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *n1ParentModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestReproN1ParallelChildApprovalsRouteByNS: two foreground Tasks in one
// batch fan out as two Send tasks; both children pause on their Write ask in
// ONE superstep, so the pause carries both interrupts and the answer is a
// map keyed by each interrupt's NS. Each child must receive ITS OWN answer:
// the allowed child's file lands, the denied child's file never does (and its
// report still backfills). Alternates the allow/deny roles per iteration;
// 40 iterations with 0 misroutes (the archived relay defect misrouted 35/40).
func TestReproN1ParallelChildApprovalsRouteByNS(t *testing.T) {
	const iterations = 40
	misrouted := 0
	for it := range iterations {
		workdir := t.TempDir()
		a, err := New(Config{
			Model:          &n1ParentModel{},
			SubagentModel:  &promptFileModel{},
			Registry:       tools.NewRegistry(workdir),
			SystemPrompt:   "t",
			PermissionMode: config.ModeDefault,
			WorkDir:        workdir,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		thread := fmt.Sprintf("thread-repro-n1-%d", it)
		intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("go")}, thread))
		if len(intrs) != 2 {
			t.Fatalf("iter %d: pauses = %d, want 2 (both children's Write asks in one superstep); first payload = %#v", it, len(intrs), DecodeInterruptValue(intrs[0].Value))
		}
		if intrs[0].NS == "" || intrs[0].NS == intrs[1].NS {
			t.Fatalf("iter %d: interrupt NSes %q / %q must be present and distinct", it, intrs[0].NS, intrs[1].NS)
		}
		// Allow one child's Write, deny the other's; alternate the roles so
		// both directions are covered across the sweep.
		allowFile, denyFile := "a.txt", "b.txt"
		if it%2 == 1 {
			allowFile, denyFile = denyFile, allowFile
		}
		resume := map[string]any{}
		for _, intr := range intrs {
			req, ok := DecodeInterruptValue(intr.Value).(ApprovalRequest)
			if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
				t.Fatalf("iter %d: child pause payload = %#v, want a one-ask Write ApprovalRequest", it, DecodeInterruptValue(intr.Value))
			}
			askFile, _ := req.Asks[0].ToolArgs["file_path"].(string)
			outcome := OutcomeDeny
			if askFile == allowFile {
				outcome = OutcomeAllow
			}
			resume[intr.NS] = ApprovalAnswer{Decisions: map[string]AskDecision{
				req.Asks[0].CallID: {Outcome: outcome},
			}}
		}
		intrs, final := pumpStream(t, a.ResumeStream(ctx, thread, resume))
		if len(intrs) != 0 {
			t.Fatalf("iter %d: re-paused on %#v, want completion after the map answer", it, DecodeInterruptValue(intrs[0].Value))
		}
		// Both reports backfilled to their own call ids.
		var reportA, reportB bool
		for _, m := range final {
			if m.Role != messages.RoleTool {
				continue
			}
			if m.ToolCallID == "tA" && strings.Contains(m.Content, "sub done") {
				reportA = true
			}
			if m.ToolCallID == "tB" && strings.Contains(m.Content, "sub done") {
				reportB = true
			}
		}
		if !reportA || !reportB {
			t.Fatalf("iter %d: reports missing (tA=%v tB=%v); transcript: %v", it, reportA, reportB, final)
		}
		_, allowErr := os.Stat(filepath.Join(workdir, allowFile))
		_, denyErr := os.Stat(filepath.Join(workdir, denyFile))
		if allowErr != nil || denyErr == nil {
			misrouted++
			if misrouted == 1 {
				t.Logf("iter %d: allowed %q (exists=%v) denied %q (exists=%v) — the answers crossed children", it, allowFile, allowErr == nil, denyFile, denyErr == nil)
			}
		}
	}
	t.Logf("misrouted approvals: %d / %d iterations", misrouted, iterations)
	if misrouted > 0 {
		t.Errorf("CONSENT MISROUTE: %d/%d iterations applied one child's answer to the other child", misrouted, iterations)
	}
}
