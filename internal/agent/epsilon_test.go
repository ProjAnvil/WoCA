package agent

// Task 9 (ε-min, design §3.5): the revived R2/R3 cross-process repro tests,
// adapted from the 2026-11-08 relay-idempotency analysis archive to the
// graph-native topology. Both now expect interrupted markers and ZERO
// re-execution across the restart: counters stay at their pre-crash values
// and no committed approval is reused as consent to execute.
//
// The archived tests produced their pauses with the RELAY shape (a Task tool
// pausing its child), which no longer exists. Each revival therefore builds
// its paused thread with a minimal old-shape graph over a durable sqlite
// saver — the same hand-crafted-checkpoint technique as
// TestLegacyToolsPauseMarkedInterrupted (Task 5):
//
//   - R2 is the CRASH face: the thread is left at a completed approval /
//     unfinished tools superstep (Next=[tools], committed Run decisions, no
//     pending interrupt, no tool results in state) — exactly the shape the
//     fork leaves when a process dies mid-execution-superstep, because tool
//     results persist only at superstep commit and the aborted run saves
//     nothing. A ctx-cancel cannot deterministically produce this (the tools
//     node converts tool errors into result messages and commits), so the
//     checkpoint is crafted by failing the node after the side effect lands.
//   - R3 is the LEGACY face: a relay-era pause whose pending interrupt is the
//     SubagentInterrupt envelope (with decisions committed — the Task-5/6
//     intermediate shape whose plain resume would fall into runTask's
//     "routed to the tools node" tombstone). The counting tool's pre-pause
//     execution rode the relay's in-memory batch memo and died with process
//     1, so the batch's calls are unanswered in the checkpoint.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/cpserde"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

// crashMarkerText is the ε-min marker for calls whose execution superstep
// never completed (kept a literal here so the red test fails on assertions,
// not on a missing symbol).
const crashMarkerText = "Error: interrupted before completion; the tool may or may not have run. Verify before retrying."

// epsilonBatch is the shared parent batch of both repros: an allow-ruled
// probe plus a foreground Task, mirroring the archived R2/R3 batches.
func epsilonBatch() []messages.ToolCall {
	return []messages.ToolCall{
		{ID: "c1", Name: "memo_probe", Args: map[string]any{}},
		{ID: "t1", Name: "Task", Args: map[string]any{"description": "write", "prompt": "write the file"}},
	}
}

// epsilonCheckpointGraph builds the minimal old-shape graph both repros
// craft their paused thread with: model → approval → tools, the approval
// node committing Run decisions for the whole batch (the completed approval
// whose reuse must never happen). The tools node's behavior is the face
// under test and is supplied by toolsFn; the invoke's outcome (a pause for
// the legacy face, an abort for the crash face) is returned for the caller
// to assert.
func epsilonCheckpointGraph(t *testing.T, dbPath, thread string, calls []messages.ToolCall, toolsFn lggraph.NodeFunc) (lggraph.Result, error) {
	t.Helper()
	saver, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = saver.Close() }()
	g := lggraph.NewStateGraph()
	g.AddReducer("messages", messagesChannel)
	g.AddReducer(decisionsChannelName, decisionsChannel)
	g.AddNode("model", func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		return nil, nil // the input history already carries the AI batch
	})
	g.AddNode("approval", func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		per := make(map[string]callDecision, len(calls))
		for _, c := range calls {
			per[c.ID] = callDecision{Run: true}
		}
		return map[string]any{decisionsChannelName: decisionsState{PerCall: per}}, nil
	})
	g.AddNode("tools", toolsFn)
	g.AddEdge("approval", "tools")
	g.AddEdge("tools", "model")
	g.AddConditionalEdges("model", func(rt lgruntime.Runtime, state map[string]any) ([]any, error) {
		msgs := MessagesFrom(state)
		if len(msgs) == 0 {
			return []any{lgtypes.END}, nil
		}
		last := msgs[len(msgs)-1]
		if last.Role == messages.RoleAI && len(last.ToolCalls) > 0 {
			return []any{"approval"}, nil
		}
		return []any{lgtypes.END}, nil
	})
	g.SetEntryPoint("model")
	cg, err := g.Compile(lggraph.WithCheckpointer(saver))
	if err != nil {
		t.Fatalf("compile checkpoint graph: %v", err)
	}
	msgs := []messages.Message{
		messages.Human("do it"),
		{Role: messages.RoleAI, ToolCalls: calls},
	}
	return cg.InvokeWithOptions(t.Context(), map[string]any{"messages": msgs}, lggraph.Options{ThreadID: thread})
}

// R2 — crash face, cross-process. Process 1 dies mid-tools-superstep after
// the probe's side effect landed but before any result persisted; process 2
// (fresh agent, same sqlite file) resumes. Want: interrupted markers for the
// whole batch, the probe counter frozen at its pre-crash value, no tombstone,
// and the run finished (not re-paused).
func TestReproR2CrashFaceCrossProcessMarkers(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	var n atomic.Int32
	thread := "thread-repro-r2-graph"
	calls := epsilonBatch()
	_, cpErr := epsilonCheckpointGraph(t, dbPath, thread, calls, func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		// The probe's side effect lands, then the "process dies": the node
		// fails before any tool result is written, so the thread's latest
		// checkpoint stays at the approval boundary (Next=[tools], committed
		// Run decisions, no pending interrupt).
		n.Add(1)
		return nil, fmt.Errorf("simulated crash mid-tools-superstep")
	})
	if cpErr == nil || !strings.Contains(cpErr.Error(), "simulated crash") {
		t.Fatalf("checkpoint graph should have aborted mid-tools, got err=%v", cpErr)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("pre-crash memo_probe executions = %d, want 1", got)
	}

	s2, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	model := &transcriptModel{script: []messages.Message{
		{Role: messages.RoleAI, ToolCalls: calls},
		messages.AI("parent done"),
		messages.AI("parent done"),
	}}
	a2, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		ExtraTools:     []coretools.Tool{memoProbeTool(&n)},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"memo_probe"}},
		WorkDir:        workdir,
		Checkpointer:   s2,
	})
	if err != nil {
		t.Fatal(err)
	}
	final, err := a2.Resume(t.Context(), thread, "y")
	if err != nil {
		t.Fatalf("cross-process resume of the crashed thread: %v", err)
	}
	logToolMsgs(t, "final", final)
	if got := n.Load(); got != 1 {
		t.Errorf("CRASH RE-EXECUTION: memo_probe executed %d times across the restart, want 1 (the pre-crash count)", got)
	}
	for _, id := range []string{"c1", "t1"} {
		m, ok := taskResultFor(final, id)
		if !ok {
			t.Errorf("call %s has no tool result in the final transcript", id)
			continue
		}
		if m.Content != crashMarkerText {
			t.Errorf("call %s result = %q, want the interrupted marker %q", id, m.Content, crashMarkerText)
		}
	}
	if toolContentContains(final, "routed to the tools node") {
		t.Errorf("tombstone surfaced: the Task call fell into runTask's routing tombstone")
	}
	if last := final[len(final)-1]; last.Role != messages.RoleAI || last.Content != "parent done" {
		t.Errorf("run did not close the turn; last message = %+v", final[len(final)-1])
	}
}

// R3 — legacy face, cross-process, streaming entry. Process 1 pauses on the
// relay-era SubagentInterrupt envelope after the counting tool executed once
// (its result rode the relay's in-memory batch memo and died with process
// 1); process 2 resumes through ResumeStream. Want: interrupted markers for
// the whole unanswered batch, the counter frozen at 1, no tombstone, and the
// stream finishing without a pause.
func TestReproR3LegacySubagentPauseCrossProcessMarkers(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	var gatedRuns atomic.Int32
	thread := "thread-repro-r3-graph"
	calls := []messages.ToolCall{
		{ID: "g1", Name: "counting_tool", Args: map[string]any{}},
		{ID: "p1", Name: "Task", Args: map[string]any{"description": "write", "prompt": "write the file"}},
	}
	cpRes, cpErr := epsilonCheckpointGraph(t, dbPath, thread, calls, func(rt lgruntime.Runtime, state map[string]any) (any, error) {
		// The approved counting tool executes (its result never persists —
		// the relay memo died with the process), then the Task relay pauses
		// on the child's Write ask with the legacy envelope.
		gatedRuns.Add(1)
		lggraph.Interrupt(rt, encodeInterruptPayload(SubagentInterrupt{
			Subagent:   "general",
			ToolCallID: "p1",
			ThreadID:   "child-thread-r3",
			Inner:      PermissionRequest{ToolName: "Write", Reason: "child write"},
		}))
		return nil, nil
	})
	if cpErr != nil {
		t.Fatalf("checkpoint graph invoke: %v", cpErr)
	}
	if len(cpRes.Interrupts) != 1 {
		t.Fatalf("checkpoint graph pauses = %d, want 1 (the relayed child ask)", len(cpRes.Interrupts))
	}
	if _, ok := DecodeInterruptValue(cpRes.Interrupts[0].Value).(SubagentInterrupt); !ok {
		t.Fatalf("pause payload = %T, want SubagentInterrupt", DecodeInterruptValue(cpRes.Interrupts[0].Value))
	}
	if got := gatedRuns.Load(); got != 1 {
		t.Fatalf("pre-restart counting_tool executions = %d, want 1", got)
	}

	s2, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	counting, err := coretools.FromFunc("counting_tool", "test counting tool",
		func(ctx context.Context, args struct{}) (coretools.Result, error) {
			gatedRuns.Add(1)
			return coretools.Result{Content: fmt.Sprintf("gated-run-%d", gatedRuns.Load())}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	model := &transcriptModel{script: []messages.Message{
		{Role: messages.RoleAI, ToolCalls: calls},
		messages.AI("done"),
		messages.AI("done"),
	}}
	a2, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		ExtraTools:     []coretools.Tool{counting},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		Permissions:    &permissions.Rules{Allow: []string{"counting_tool"}},
		WorkDir:        workdir,
		Checkpointer:   s2,
	})
	if err != nil {
		t.Fatal(err)
	}
	intrs, final := pumpStream(t, a2.ResumeStream(t.Context(), thread, "r2"))
	if len(intrs) != 0 {
		t.Fatalf("resume re-paused: %#v", DecodeInterruptValue(intrs[0].Value))
	}
	logToolMsgs(t, "final", final)
	if got := gatedRuns.Load(); got != 1 {
		t.Errorf("CROSS-PROCESS RE-EXECUTION: approved counting_tool executed %d times across the restart, want 1 (the pre-restart count)", got)
	}
	for _, id := range []string{"g1", "p1"} {
		m, ok := taskResultFor(final, id)
		if !ok {
			t.Errorf("call %s has no tool result in the final transcript", id)
			continue
		}
		if m.Content != legacyInterruptedMarker {
			t.Errorf("call %s result = %q, want the legacy interrupted marker %q", id, m.Content, legacyInterruptedMarker)
		}
	}
	if toolContentContains(final, "routed to the tools node") {
		t.Errorf("tombstone surfaced: the Task call fell into runTask's routing tombstone")
	}
	if last := final[len(final)-1]; last.Role != messages.RoleAI || last.Content != "done" {
		t.Errorf("run did not close the turn; last message = %+v", final[len(final)-1])
	}
}
