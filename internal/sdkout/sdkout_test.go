package sdkout

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/tools"
)

// runFakeAgent drives an agent whose model issues one tool call then answers.
func runFakeAgent(t *testing.T) ([]graph.Step, []messages.Message, *agent.Agent) {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "t1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("all done"),
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the steps the runner would have rendered: state updates per
	// node transition.
	var steps []graph.Step
	steps = append(steps, graph.Step{Update: graph.State{"messages": []messages.Message{out[2]}}}) // AI tool call
	steps = append(steps, graph.Step{Update: graph.State{"messages": []messages.Message{out[3]}}}) // tool result
	return steps, out, a
}

func TestStreamJSON(t *testing.T) {
	steps, out, a := runFakeAgent(t)
	var b strings.Builder
	w, err := New(FormatStreamJSON, &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		w.Step(s)
	}
	w.Final(out, a.Usage(), "claude-sonnet-4-5", 2, nil)

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d:\n%s", len(lines), b.String())
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["type"] != "assistant" {
		t.Fatalf("first event = %v", ev)
	}
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["type"] != "user" {
		t.Fatalf("second event = %v", ev)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &result); err != nil {
		t.Fatal(err)
	}
	if result["type"] != "result" || result["subtype"] != "success" || result["result"] != "all done" {
		t.Fatalf("result event = %v", result)
	}
	if result["num_turns"] != float64(2) {
		t.Fatalf("num_turns = %v", result["num_turns"])
	}
	if _, ok := result["total_cost_usd"]; !ok {
		t.Fatal("cost estimate missing for a known model")
	}
	if _, ok := result["usage"]; !ok {
		t.Fatal("usage missing")
	}
}

func TestJSONFormatSingleObject(t *testing.T) {
	_, out, a := runFakeAgent(t)
	var b strings.Builder
	w, err := New(FormatJSON, &b)
	if err != nil {
		t.Fatal(err)
	}
	// Steps are ignored in json mode.
	for _, s := range steps(out) {
		w.Step(s)
	}
	w.Final(out, a.Usage(), "unknown-model-x", 1, nil)

	sc := bufio.NewScanner(strings.NewReader(b.String()))
	count := 0
	var result map[string]any
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		count++
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			t.Fatalf("invalid json line %q: %v", line, err)
		}
	}
	if count != 1 {
		t.Fatalf("json format must emit exactly one object, got %d", count)
	}
	if result["result"] != "all done" || result["type"] != "result" {
		t.Fatalf("result = %v", result)
	}
	if _, ok := result["total_cost_usd"]; ok {
		t.Fatal("unknown model must omit the cost estimate")
	}
}

func TestErrorResult(t *testing.T) {
	var b strings.Builder
	w, _ := New(FormatJSON, &b)
	w.Final(nil, agent.UsageTotals{}, "m", 0, errBoom)
	var ev map[string]any
	if err := json.Unmarshal([]byte(b.String()), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["is_error"] != true || ev["subtype"] != "error" || ev["error"] != "boom" {
		t.Fatalf("error event = %v", ev)
	}
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom = boomError{}

func steps(out []messages.Message) []graph.Step {
	return []graph.Step{{Update: graph.State{"messages": out}}}
}
