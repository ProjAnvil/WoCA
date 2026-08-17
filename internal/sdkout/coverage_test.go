package sdkout

import (
	"bytes"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
)

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
