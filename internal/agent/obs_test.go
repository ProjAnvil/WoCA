package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/tools"
)

// TestAgentGraphObservedEndToEnd runs one agent turn with the observability
// store enabled and verifies the graph-node events and message dump land in
// the workspace/session/message layout.
func TestAgentGraphObservedEndToEnd(t *testing.T) {
	root := t.TempDir()
	obs := obslog.New(root, false)
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Read", Args: map[string]any{"file_path": "x"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Obs:            obs,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enable mid-flight (the live toggle path) and scope a run.
	obs.SetEnabled(true)
	a.SetObsRun("/tmp/demo-project", "sess-obs-1", "run-obs-1")
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("read x")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	obs.DumpMessages(out, nil)

	// The workspace dir name includes a path hash suffix; find it via glob.
	trace := ""
	match := filepath.Join(root, "ws-demo-project-*", "sess-obs-1.jsonl")
	found, _ := filepath.Glob(match)
	if len(found) != 1 {
		t.Fatalf("session trace not found via %s: %v", match, found)
	}
	trace = found[0]

	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type ev struct {
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	var events []ev
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("parse: %v", err)
		}
		events = append(events, e)
	}

	var starts, ends int
	sawModel, sawTools := false, false
	for _, e := range events {
		if e.Event != "graph_node" {
			continue
		}
		switch e.Data["phase"] {
		case "start":
			starts++
		case "end":
			ends++
		}
		switch e.Data["node"] {
		case "model":
			sawModel = true
		case "tools":
			sawTools = true
		}
		if e.Data["graph"] != "agent" {
			t.Fatalf("wrong graph tag: %+v", e.Data)
		}
	}
	if !sawModel || !sawTools {
		t.Fatalf("missing node events (model=%v tools=%v): %+v", sawModel, sawTools, events)
	}
	if starts == 0 || ends == 0 {
		t.Fatalf("missing lifecycle phases: %d starts, %d ends", starts, ends)
	}

	// Message dump exists with the full transcript.
	dump := filepath.Join(filepath.Dir(trace), "sess-obs-1", "run-obs-1.json")
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("message dump missing: %v", err)
	}
	var payload struct {
		Messages []messages.Message `json:"messages"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) < 4 {
		t.Fatalf("dump messages = %d", len(payload.Messages))
	}
}
