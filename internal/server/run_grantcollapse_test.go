package server

// T5 Part A full-stack repros (e2e hitl "two parallel child approvals"): two
// fanned-out children park on same-rule-key permission asks in one superstep.
// The e2e wedges at HEAD (answer 200, stack closes, run never finishes) —
// these are the in-process twins over the real server stack (run loop,
// handleInterrupt parks, /answer decoding, session-grant collapse).

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"net/http/httptest"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/tools"
)

// t5ParentModel re-issues the two-Task batch until both call ids have tool
// results, then reports done (transcript-driven; see subagentSectionModel for
// why the fan-out needs one).
type t5ParentModel struct{}

func (m *t5ParentModel) pick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	if answered["tA"] && answered["tB"] {
		return messages.AI("Fan-out complete.")
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "tA", Name: "Task", Args: map[string]any{"description": "a", "prompt": "FILE-A write it"}},
		{ID: "tB", Name: "Task", Args: map[string]any{"description": "b", "prompt": "FILE-B write it"}},
	}}
}

func (m *t5ParentModel) Invoke(ctx context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *t5ParentModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *t5ParentModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *t5ParentModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *t5ParentModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *t5ParentModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *t5ParentModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// t5ChildModel writes pkg/<a|b>.txt (same grant directory — the session rule
// keys collapse) then reports done.
type t5ChildModel struct{}

func (m *t5ChildModel) pick(input []messages.Message) messages.Message {
	prompt := ""
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
		if msg.Role == messages.RoleHuman && prompt == "" {
			prompt = msg.Content
		}
	}
	file := "pkg/a.txt"
	if strings.Contains(prompt, "FILE-B") {
		file = "pkg/b.txt"
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "w-" + filepath.Base(file), Name: "Write", Args: map[string]any{"file_path": file, "content": "x"}},
	}}
}

func (m *t5ChildModel) Invoke(ctx context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *t5ChildModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *t5ChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *t5ChildModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *t5ChildModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *t5ChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *t5ChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// t5Server builds the two-children fan-out server. cp, when non-empty,
// enables the durable sqlite checkpoint dir (the e2e serve form).
func t5Server(t *testing.T, cp string) (*httptest.Server, string) {
	t.Helper()
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &t5ParentModel{},
		SubagentModel:  &t5ChildModel{},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
		CheckpointDir:  cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	return ts, workdir
}

// t5SSE is one SSE event of a run stream.
type t5SSE struct {
	Type string
	Data string
}

// t5Events subscribes to a run's event stream.
func t5Events(t *testing.T, base, runID string, timeout time.Duration) <-chan t5SSE {
	t.Helper()
	out := make(chan t5SSE, 64)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(out)
		defer resp.Body.Close()
		defer cancel()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for {
			ev, ok := readSSEEvent(sc)
			if !ok {
				return
			}
			out <- t5SSE{Type: ev.Type, Data: ev.Data}
		}
	}()
	return out
}

// t5WaitInterrupt waits for the next interrupt event and returns its cards
// (ns/id/kind), failing on terminal events.
func t5WaitInterrupt(t *testing.T, events <-chan t5SSE, timeout time.Duration) []t5Card {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream ended before the interrupt")
			}
			switch ev.Type {
			case "interrupt":
				var probe struct {
					Kind  string   `json:"kind"`
					Cards []t5Card `json:"cards"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &probe); err != nil {
					t.Fatalf("bad interrupt payload: %v", err)
				}
				if len(probe.Cards) == 0 {
					t.Fatalf("interrupt carries no cards: %s", ev.Data)
				}
				return probe.Cards
			case "done", "error":
				t.Fatalf("run ended before the interrupt: %s %s", ev.Type, ev.Data)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the interrupt")
		}
	}
}

// t5Card is one parsed interrupt card of a park.
type t5Card struct {
	NS   string `json:"ns"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// t5WaitDone waits for the terminal done event, failing on error or timeout.
func t5WaitDone(t *testing.T, events <-chan t5SSE, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream ended before done")
			}
			switch ev.Type {
			case "done":
				return
			case "error":
				t.Fatalf("run error: %s", ev.Data)
			}
		case <-deadline:
			t.Fatal("timed out waiting for done (wedged run)")
		}
	}
}

// TestT5GrantCollapseCompletesRun: the e2e's step (2) — a session-grant
// answer on ONE card must collapse the same-rule-key sibling and the run must
// complete.
func TestT5GrantCollapseCompletesRun(t *testing.T) {
	ts, workdir := t5Server(t, "")
	defer ts.Close()
	runID, _ := createRun(t, ts.URL, "")
	events := t5Events(t, ts.URL, runID, 60*time.Second)

	cards := t5WaitInterrupt(t, events, 30*time.Second)
	if len(cards) != 2 {
		t.Fatalf("parked %d cards, want 2", len(cards))
	}

	// Grant-session on the first card only; the server collapses the sibling.
	body := `{"answers":[{"key":"` + cards[0].NS + `","value":{"decisions":{"w-a.txt":{"outcome":"allow","grant":"session"}}}}]}`
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", body); err != nil {
		t.Fatalf("grant answer: %v", err)
	}
	t5WaitDone(t, events, 30*time.Second)

	for _, f := range []string{"pkg/a.txt", "pkg/b.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Fatalf("approved write %s did not execute: %v", f, err)
		}
	}
}

// TestT5PartialThenSiblingDenyCompletes: the e2e's step (5) — allow ONE card
// (partial answer), then deny the re-parked sibling; the run must complete
// with the denied child reporting. Runs in both checkpoint modes: the e2e
// serve uses the durable sqlite dir.
func TestT5PartialThenSiblingDenyCompletes(t *testing.T) {
	for _, cp := range []string{"", t.TempDir()} {
		ts, _ := t5Server(t, cp)
		runID, _ := createRun(t, ts.URL, "")
		events := t5Events(t, ts.URL, runID, 60*time.Second)

		cards := t5WaitInterrupt(t, events, 30*time.Second)
		if len(cards) != 2 {
			t.Fatalf("cp=%q: parked %d cards, want 2", cp, len(cards))
		}

		// Partial answer: allow the first card only; the second re-parks.
		allow := `{"answers":[{"key":"` + cards[0].NS + `","value":{"approve":true}}]}`
		if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", allow); err != nil {
			t.Fatalf("cp=%q: allow answer: %v", cp, err)
		}
		reParked := t5WaitInterrupt(t, events, 30*time.Second)
		if len(reParked) != 1 {
			t.Fatalf("cp=%q: re-parked %d cards, want 1 (the answered child must NOT re-ask): %+v", cp, len(reParked), reParked)
		}

		// Deny the re-parked sibling by its CURRENT card key.
		deny := `{"answers":[{"key":"` + t5CardKey(reParked[0]) + `","value":{"approve":false}}]}`
		if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", deny); err != nil {
			t.Fatalf("cp=%q: deny answer: %v", cp, err)
		}
		t5WaitDone(t, events, 30*time.Second)
		ts.Close()
	}
}

func t5CardKey(c t5Card) string {
	if c.NS != "" {
		return c.NS
	}
	return c.ID
}

// t5SlowChildModel is t5ChildModel with a SLOW gated call (Bash sleep) —
// the e2e's npm-install timing: after the partial allow, the answered child
// spends real time in its tools node, so the sibling's deny can land in the
// resume window (before the re-park), exactly like the browser UI's
// optimistic card.
type t5SlowChildModel struct{}

func (m *t5SlowChildModel) pick(input []messages.Message) messages.Message {
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
		{ID: "w-" + file, Name: "Bash", Args: map[string]any{"command": "sleep 1 && touch " + file}},
	}}
}

func (m *t5SlowChildModel) Invoke(ctx context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *t5SlowChildModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *t5SlowChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *t5SlowChildModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *t5SlowChildModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *t5SlowChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *t5SlowChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestT5DenyInsideResumeWindowTwice: the e2e/curl repro's trigger is the
// SECOND run on one serve: the first deny-flow run completes, the next one
// loses the partial allow (the answered child re-runs fresh under a new NS
// and re-asks; the run wedges on a card nobody can answer). Both rounds must
// complete.
func TestT5DenyInsideResumeWindowTwice(t *testing.T) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &t5ParentModel{},
		SubagentModel:  &t5SlowChildModel{},
		Registry:       tools.NewRegistry(workdir),
		Exec:           executor.NewLocal(),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
		CheckpointDir:  t.TempDir(), // the e2e serve's durable sqlite form
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	defer ts.Close()

	for round := 1; round <= 2; round++ {
		runID, _ := createRun(t, ts.URL, "")
		events := t5Events(t, ts.URL, runID, 60*time.Second)
		cards := t5WaitInterrupt(t, events, 30*time.Second)
		if len(cards) != 2 {
			t.Fatalf("round %d: parked %d cards, want 2", round, len(cards))
		}
		allow := `{"answers":[{"key":"` + cards[0].NS + `","value":{"decisions":{"w-a.txt":{"outcome":"allow"}}}}]}`
		if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", allow); err != nil {
			t.Fatalf("round %d: allow answer: %v", round, err)
		}
		deny := `{"answers":[{"key":"` + cards[1].NS + `","value":{"decisions":{"w-b.txt":{"outcome":"deny"}}}}]}`
		if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/answer", deny); err != nil {
			t.Fatalf("round %d: deny answer: %v", round, err)
		}
		t5WaitDone(t, events, 30*time.Second)
	}
}
