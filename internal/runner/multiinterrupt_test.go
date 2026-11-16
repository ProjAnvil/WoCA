package runner

// Task 8: the driver-side interrupt-list contract. A pause's FULL interrupt
// list reaches the handler as cards (fork NS + decoded value); the answer is
// a scalar for one pending card and an NS-keyed map for several; the goal
// path unwraps the relay's aggregate and wraps the map answer under the
// outer goal interrupt's key.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// statJoin stats workdir/name (test brevity).
func statJoin(workdir, name string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(workdir, name))
}

// scriptModel adapts a pick function into a ChatModel.
type scriptModel struct {
	pick func(input []messages.Message) messages.Message
}

func (m *scriptModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *scriptModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *scriptModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *scriptModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *scriptModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *scriptModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *scriptModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// taskFanoutPick answers from the transcript: Task calls for every id
// without a tool result, else a plain reply. The child prompt is
// "FILE-<X> write it" for id "t<x>".
func taskFanoutPick(input []messages.Message, doneText string, ids ...string) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	calls := make([]messages.ToolCall, 0, len(ids))
	done := true
	for _, id := range ids {
		if !answered[id] {
			done = false
		}
		calls = append(calls, messages.ToolCall{ID: id, Name: "Task", Args: map[string]any{
			"description": id, "prompt": "FILE-" + strings.ToUpper(strings.TrimPrefix(id, "t")) + " write it",
		}})
	}
	if done {
		return messages.AI(doneText)
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: calls}
}

// childWriteAskPick is the subagent child: the first round asks a Write for
// the file named by the prompt (FILE-A → a.txt); later rounds report done.
func childWriteAskPick(input []messages.Message) messages.Message {
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
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "w-" + file, Name: "Write", Args: map[string]any{"file_path": file, "content": "x"}},
	}}
}

// TestRunnerMultiInterruptCardsAndMapResume: two children pause in one
// superstep; the handler receives BOTH cards (distinct NS, decoded
// ApprovalRequest values) and its NS-keyed map answer routes each child's
// verdict to its own ask.
func TestRunnerMultiInterruptCardsAndMapResume(t *testing.T) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &scriptModel{pick: func(in []messages.Message) messages.Message { return taskFanoutPick(in, "parent done", "tA", "tB") }},
		SubagentModel:  &scriptModel{pick: childWriteAskPick},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	var handled [][]agent.PauseCard
	r := New(a, func(cards []agent.PauseCard) (any, error) {
		handled = append(handled, cards)
		if len(cards) != 2 {
			t.Fatalf("handler received %d cards, want 2 (the pause's full interrupt list)", len(cards))
		}
		answer := map[string]any{}
		for _, card := range cards {
			req, ok := card.Value.(agent.ApprovalRequest)
			if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
				t.Fatalf("card value = %#v, want a one-ask Write ApprovalRequest", card.Value)
			}
			if card.NS == "" {
				t.Fatalf("card %v carries no NS — map answers could not address it", card)
			}
			file, _ := req.Asks[0].ToolArgs["file_path"].(string)
			outcome := agent.OutcomeDeny
			if file == "a.txt" {
				outcome = agent.OutcomeAllow
			}
			answer[card.NS] = agent.ApprovalAnswer{Decisions: map[string]agent.AskDecision{
				req.Asks[0].CallID: {Outcome: outcome},
			}}
		}
		return answer, nil
	}, nil)
	final, err := r.Run(t.Context(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(handled) != 1 {
		t.Fatalf("handler ran %d times, want 1 (the map answered both cards)", len(handled))
	}
	if _, err := statJoin(workdir, "a.txt"); err != nil {
		t.Fatal("allowed child's Write did not execute")
	}
	if _, err := statJoin(workdir, "b.txt"); err == nil {
		t.Fatal("denied child's Write executed")
	}
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
		t.Fatalf("reports missing (tA=%v tB=%v): %v", reportA, reportB, final)
	}
}

// goalFanoutPick scripts the goal loop's model: assess and evaluate answer
// their focused prompts; execute rounds fan out two Tasks until both
// reports are back.
func goalFanoutPick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	lastHuman := ""
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
		if msg.Role == messages.RoleHuman {
			lastHuman = msg.Content
		}
	}
	switch {
	case strings.Contains(lastHuman, "skeptical evaluator"):
		return messages.AI(`{"verdict":"complete","confidence":0.9,"evidence":"both child tasks settled"}`)
	case strings.Contains(lastHuman, "planning stage"):
		return messages.AI("status: starting\nThis cycle: fan out the two file tasks")
	}
	return taskFanoutPick(input, "all done", "tA", "tB")
}

// TestRunnerGoalMultiInterruptCardsAndWrap: RunGoal unwraps the relay's
// aggregate into cards, and its map answer — wrapped under the outer goal
// interrupt's key — resumes both children.
func TestRunnerGoalMultiInterruptCardsAndWrap(t *testing.T) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &scriptModel{pick: goalFanoutPick},
		SubagentModel:  &scriptModel{pick: childWriteAskPick},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetGoal("write both files", "", 5, 1_000_000); err != nil {
		t.Fatal(err)
	}
	var handled [][]agent.PauseCard
	r := New(a, func(cards []agent.PauseCard) (any, error) {
		handled = append(handled, cards)
		if len(cards) != 2 {
			t.Fatalf("handler received %d cards, want 2 (the unwrapped goal aggregate)", len(cards))
		}
		answer := map[string]any{}
		for _, card := range cards {
			req := card.Value.(agent.ApprovalRequest)
			file, _ := req.Asks[0].ToolArgs["file_path"].(string)
			outcome := agent.OutcomeAllow
			if file == "b.txt" {
				outcome = agent.OutcomeDeny
			}
			answer[card.NS] = agent.ApprovalAnswer{Decisions: map[string]agent.AskDecision{
				req.Asks[0].CallID: {Outcome: outcome},
			}}
		}
		return answer, nil
	}, nil)
	if _, err := r.RunGoal(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(handled) != 1 {
		t.Fatalf("handler ran %d times, want 1", len(handled))
	}
	if g := a.Goal(); g.Status != agent.GoalComplete {
		t.Fatalf("goal status = %q, want complete", g.Status)
	}
	if _, err := statJoin(workdir, "a.txt"); err != nil {
		t.Fatal("allowed child's file missing")
	}
	if _, err := statJoin(workdir, "b.txt"); err == nil {
		t.Fatal("denied child's file exists")
	}
}

// TestCardResumeValue pins the scalar/map normalization: one card accepts a
// scalar (unwrapping a single-key map addressed to it); several cards
// require a map, anything else errors.
func TestCardResumeValue(t *testing.T) {
	one := []agent.PauseCard{{NS: "ns1", ID: "id1", Value: "v"}}
	two := append(one, agent.PauseCard{NS: "ns2", ID: "id2", Value: "v"})

	if v, err := cardResumeValue(one, "y"); err != nil || v != "y" {
		t.Fatalf("scalar/one = %v, %v", v, err)
	}
	m := map[string]any{"ns1": 42}
	if v, err := cardResumeValue(one, m); err != nil || v != 42 {
		t.Fatalf("single-key map/one = %v, %v", v, err)
	}
	if v, err := cardResumeValue(one, map[string]any{"other": 42}); err != nil || v == nil {
		t.Fatalf("foreign-key map/one = %v, %v (passes through)", v, err)
	}
	if v, err := cardResumeValue(two, m); err != nil {
		t.Fatalf("map/two error: %v", err)
	} else if got, ok := v.(map[string]any); !ok || len(got) != 1 || got["ns1"] != 42 {
		t.Fatalf("map/two = %#v, want the map itself", v)
	}
	if _, err := cardResumeValue(two, "y"); err == nil {
		t.Fatal("scalar answer with two pending cards must error")
	}
}

// TestGoalResumeValue pins the goal wrap: map answers nest under the outer
// interrupt's key (NS preferred, ID for legacy NS-less pauses); scalars
// pass through.
func TestGoalResumeValue(t *testing.T) {
	inner := map[string]any{"ns-a": 1}
	if v := goalResumeValue([]lgtypes.Interrupt{{NS: "goal_execute:se1", ID: "i1"}}, inner); v == nil {
		t.Fatal("wrap returned nil")
	} else if m, ok := v.(map[string]any); !ok {
		t.Fatalf("wrap = %#v, want a map", v)
	} else if got, ok := m["goal_execute:se1"].(map[string]any); !ok || got["ns-a"] != 1 {
		t.Fatalf("wrap = %#v, want {goal_execute:se1: inner}", v)
	}
	if v := goalResumeValue([]lgtypes.Interrupt{{ID: "legacy"}}, inner); v == nil {
		t.Fatal("legacy wrap returned nil")
	} else if m, ok := v.(map[string]any); !ok {
		t.Fatalf("legacy wrap = %#v, want a map", v)
	} else if got, ok := m["legacy"].(map[string]any); !ok || got["ns-a"] != 1 {
		t.Fatalf("legacy wrap = %#v, want {legacy: inner}", v)
	}
	if v := goalResumeValue([]lgtypes.Interrupt{{NS: "n"}}, "y"); v != "y" {
		t.Fatalf("scalar wrap = %#v, want passthrough", v)
	}
	if v := goalResumeValue(nil, inner); v == nil {
		t.Fatal("no-pause wrap returned nil")
	} else if m, ok := v.(map[string]any); !ok || m["ns-a"] != 1 {
		t.Fatalf("no-pause wrap = %#v, want passthrough", v)
	}
}
