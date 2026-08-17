package agent

import (
	"context"
	"strings"
	"testing"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/memory"
)

func toolsByName(list []coretools.Tool) map[string]coretools.Tool {
	out := map[string]coretools.Tool{}
	for _, t := range list {
		out[t.Name()] = t
	}
	return out
}

func TestMemoryToolsMainIncludesWrite(t *testing.T) {
	a := &Agent{mem: memoryRuntime{engine: &fakeEngine{}}, workdir: "/ws"}
	got := toolsByName(a.memoryTools())
	for _, name := range []string{"recall_memory", "search_code", "remember_fact", "index_code"} {
		if got[name] == nil {
			t.Fatalf("missing tool %s", name)
		}
	}
}

func TestMemoryToolsSubagentReadOnly(t *testing.T) {
	a := &Agent{mem: memoryRuntime{engine: &fakeEngine{}}, workdir: "/ws", subagent: true}
	got := toolsByName(a.memoryTools())
	if got["recall_memory"] == nil || got["search_code"] == nil {
		t.Fatal("subagent should keep read tools")
	}
	if got["remember_fact"] != nil || got["index_code"] != nil {
		t.Fatal("subagent must not get write tools")
	}
}

func TestMemoryToolsNilEngine(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	if list := a.memoryTools(); len(list) != 0 {
		t.Fatalf("nil engine should yield no tools, got %d", len(list))
	}
}

func TestRecallMemoryToolInvokesEngine(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{ID: "h1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", Score: 0.9}}}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	tool := toolsByName(a.memoryTools())["recall_memory"]
	res, err := tool.Invoke(context.Background(), map[string]any{"query": "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "[L2_semantic|fact|0.90] JWT") {
		t.Fatalf("result = %q", res.Content)
	}
	if len(f.recordedRecalls()) == 0 {
		t.Fatal("engine not called")
	}
}

func TestRememberFactToolUsesWorkspaceAndSource(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.SetMemoryWorkspace("/ws/tool")
	tool := toolsByName(a.memoryTools())["remember_fact"]
	res, err := tool.Invoke(context.Background(), map[string]any{"content": "prefers tabs", "tags": []any{"pref"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "stored") {
		t.Fatalf("result = %q", res.Content)
	}
	if len(f.remembered) != 1 || f.remembered[0] != "prefers tabs" {
		t.Fatalf("remembered = %v", f.remembered)
	}
	// The engine must see the scoped workspace and the tool's source tag.
	if calls := f.recordedRememberCalls(); len(calls) != 1 || calls[0] != "/ws/tool\x00woca:tool\x00prefers tabs" {
		t.Fatalf("rememberCalls = %v", calls)
	}
	// recall_memory threads the same workspace.
	tool2 := toolsByName(a.memoryTools())["recall_memory"]
	if _, err := tool2.Invoke(context.Background(), map[string]any{"query": "x"}); err != nil {
		t.Fatal(err)
	}
	recalls := f.recordedRecalls()
	if len(recalls) == 0 || !strings.HasPrefix(recalls[len(recalls)-1], "/ws/tool|") {
		t.Fatalf("workspace not threaded: %v", recalls)
	}
}

func TestRememberFactDroppedByGate(t *testing.T) {
	f := &fakeEngine{drop: true}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	tool := toolsByName(a.memoryTools())["remember_fact"]
	res, err := tool.Invoke(context.Background(), map[string]any{"content": "duplicate"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "dropped by the attention gate") {
		t.Fatalf("result = %q", res.Content)
	}
}
