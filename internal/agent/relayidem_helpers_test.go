package agent

// Shared helpers for the revived relay-idempotency repro tests (N5 in
// relayidem_n5_test.go, X1 in sessionpolicy_test.go): transcript logging the
// repros use to document what the harness actually did.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// logToolMsgs logs every tool result of a final transcript (truncated),
// labeling the run: the repro tests' evidence trail for which calls executed
// and which were denied.
func logToolMsgs(t *testing.T, label string, msgs []messages.Message) {
	t.Helper()
	for _, m := range msgs {
		if m.Role != messages.RoleTool {
			continue
		}
		c := m.Content
		if len(c) > 160 {
			c = c[:160] + "…"
		}
		t.Logf("%s tool[%s]: %q", label, m.ToolCallID, c)
	}
}

// memoProbeTool builds the non-speculative probe used by the revived
// batch-memo and gate tests: an unknown-category tool (never speculated)
// whose result names its own execution ordinal.
func memoProbeTool(n *atomic.Int32) coretools.Tool {
	t, err := coretools.FromFunc("memo_probe", "test memo probe tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			return coretools.Result{Content: fmt.Sprintf("own-%d", n.Add(1))}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// taskCallModel is the graph-driven stand-in for the relay era's direct
// runTask unit calls: a transcript-driven parent model that emits ONE Task
// call (id "t0") with the given args and reports done once its result is
// back.
type taskCallModel struct{ args taskArgs }

func (m *taskCallModel) pick(input []messages.Message) messages.Message {
	for _, msg := range input {
		if msg.Role == messages.RoleTool && msg.ToolCallID == "t0" {
			return messages.AI("done")
		}
	}
	argsMap := map[string]any{}
	b, err := json.Marshal(m.args)
	if err == nil {
		_ = json.Unmarshal(b, &argsMap)
	}
	if len(argsMap) == 0 {
		argsMap["description"] = "do it"
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "t0", Name: "Task", Args: argsMap}}}
}

func (m *taskCallModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *taskCallModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *taskCallModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *taskCallModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *taskCallModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *taskCallModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *taskCallModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// runTaskViaGraph drives one foreground Task through the Send fan-out and
// returns the final transcript (fails on any pause).
func runTaskViaGraph(t *testing.T, a *Agent, thread string) []messages.Message {
	t.Helper()
	return runTaskViaGraphHist(t, a, thread, messages.Human("go"))
}

// runTaskViaGraphHist is runTaskViaGraph with an explicit input history.
func runTaskViaGraphHist(t *testing.T, a *Agent, thread string, hist ...messages.Message) []messages.Message {
	t.Helper()
	intrs, final := pumpStream(t, a.Stream(t.Context(), hist, thread))
	if len(intrs) != 0 {
		t.Fatalf("unexpected pause: %#v", DecodeInterruptValue(intrs[0].Value))
	}
	return final
}

// taskResultFor returns the backfilled tool result of callID.
func taskResultFor(msgs []messages.Message, callID string) (messages.Message, bool) {
	for _, m := range msgs {
		if m.Role == messages.RoleTool && m.ToolCallID == callID {
			return m, true
		}
	}
	return messages.Message{}, false
}
