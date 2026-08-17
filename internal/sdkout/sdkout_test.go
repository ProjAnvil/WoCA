package sdkout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// runFakeAgent drives an agent whose model issues one tool call then answers.
func runFakeAgent(t *testing.T) ([]lggraph.StreamChunk, []messages.Message, *agent.Agent) {
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
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the steps the runner would have rendered: state updates per
	// node transition.
	var steps []lggraph.StreamChunk
	steps = append(steps, lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": map[string]any{"messages": []messages.Message{out[2]}}}}) // AI tool call
	steps = append(steps, lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"tools": map[string]any{"messages": []messages.Message{out[3]}}}}) // tool result
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

func steps(out []messages.Message) []lggraph.StreamChunk {
	return []lggraph.StreamChunk{{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": map[string]any{"messages": out}}}}
}

func TestNewFormats(t *testing.T) {
	var out bytes.Buffer
	if _, err := New("yaml", &out); err == nil {
		t.Fatal("expected format error")
	}
	w, err := New("", &out)
	if err != nil {
		t.Fatal(err)
	}
	// Empty format behaves like json: only the final object.
	w.Step(lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": map[string]any{}}})
	if out.Len() != 0 {
		t.Fatalf("json writer should not stream steps: %q", out.String())
	}
}

func TestStreamJSONStepToolMessages(t *testing.T) {
	var out bytes.Buffer
	w, err := New(FormatStreamJSON, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Assistant with tool calls and a tool result are streamed; empty
	// assistant messages and Done steps are not.
	state := map[string]any{}
	chunks := []lggraph.StreamChunk{
		{Mode: lggraph.StreamUpdates, Payload: map[string]any{"model": state}},
	}
	for _, s := range chunks {
		w.Step(s)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"type"`)) && out.Len() > 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestLastAIText(t *testing.T) {
	if got := lastAItext(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	msgs := []messages.Message{
		messages.Human("q"),
		messages.AI(""),
		messages.AI("final answer"),
		messages.Human("again"),
	}
	if got := lastAItext(msgs); got != "final answer" {
		t.Fatalf("lastAItext = %q", got)
	}
	if got := lastAItext([]messages.Message{messages.Human("only")}); got != "" {
		t.Fatalf("no AI = %q", got)
	}
}
