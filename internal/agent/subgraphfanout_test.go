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

	lgruntime "github.com/projanvil/langchain-golang/langgraph/runtime"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/executor"
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

// TestSubgraphCapRejects: nine foreground Task calls in one batch — the
// first defaultMaxSubagents fan out, the ninth is rejected with a tool-result
// error naming the cap, never queued (design §3.2, D12).
func TestSubgraphCapRejects(t *testing.T) {
	workdir := t.TempDir()
	parent := &manyTaskModel{n: defaultMaxSubagents + 1}
	a, err := New(Config{
		Model:          parent,
		SubagentModel:  language.NewFakeChatModel(language.WithResponses(messages.AI("sub done"))),
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := runTaskViaGraphHist(t, a, "thread-cap-reject", messages.Human("go"))
	reports, rejected := 0, 0
	for _, m := range final {
		if m.Role != messages.RoleTool {
			continue
		}
		switch {
		case m.Content == "sub done":
			reports++
		case strings.Contains(m.Content, fmt.Sprintf("subagent concurrency limit reached (%d)", defaultMaxSubagents)):
			rejected++
		default:
			t.Fatalf("unexpected tool result %q", m.Content)
		}
	}
	if reports != defaultMaxSubagents || rejected != 1 {
		t.Fatalf("reports=%d rejected=%d, want %d/%d", reports, rejected, defaultMaxSubagents, 1)
	}
}

// manyTaskModel emits n Task calls (ids t0..t(n-1)) until every one has a
// tool result, then reports done.
type manyTaskModel struct{ n int }

func (m *manyTaskModel) pick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	for i := range m.n {
		if !answered[fmt.Sprintf("t%d", i)] {
			calls := make([]messages.ToolCall, 0, m.n)
			for j := range m.n {
				calls = append(calls, messages.ToolCall{ID: fmt.Sprintf("t%d", j), Name: "Task", Args: map[string]any{"description": fmt.Sprintf("job %d", j)}})
			}
			return messages.Message{Role: messages.RoleAI, ToolCalls: calls}
		}
	}
	return messages.AI("parent done")
}

func (m *manyTaskModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *manyTaskModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *manyTaskModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *manyTaskModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *manyTaskModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *manyTaskModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *manyTaskModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestSubagentShellOwnerAttribution: a background Bash started inside a
// fanned-out child lands in the session-shared shell registry with the
// child's origin as Owner (design §3.7).
func TestSubagentShellOwnerAttribution(t *testing.T) {
	workdir := t.TempDir()
	child := &bgBashChildModel{}
	a, err := New(Config{
		Model:          &taskCallModel{args: taskArgs{Description: "start a server"}},
		SubagentModel:  child,
		Registry:       tools.NewRegistry(workdir),
		Exec:           executor.NewLocal(),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := runTaskViaGraph(t, a, "thread-shell-owner")
	if res, ok := taskResultFor(final, "t0"); !ok || !strings.Contains(res.Content, "sub done") {
		t.Fatalf("child did not finish: %v", final)
	}
	shells := a.registry.Background().ListFor(sessionFromCtx(t.Context()))
	if len(shells) == 0 {
		t.Fatal("child's background shell is not visible in the session registry")
	}
	for _, sh := range shells {
		if sh.Owner == "" || sh.Owner == "main" {
			t.Fatalf("shell %s owner = %q, want the child origin", sh.ID, sh.Owner)
		}
		if !strings.HasPrefix(sh.Owner, "subagent:") {
			t.Fatalf("shell owner = %q, want subagent:<def>#<callID> form", sh.Owner)
		}
	}
	a.registry.Background().KillAll(tools.StopByUser)
}

// bgBashChildModel starts one background Bash and then reports done.
type bgBashChildModel struct{}

func (m *bgBashChildModel) pick(input []messages.Message) messages.Message {
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "bg1", Name: "Bash", Args: map[string]any{"command": "sleep 30", "run_in_background": true}},
	}}
}

func (m *bgBashChildModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *bgBashChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *bgBashChildModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *bgBashChildModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *bgBashChildModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *bgBashChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *bgBashChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestStaleFanoutErrorsLoudly (white-box): a Fanout decision naming a node
// that no longer resolves must convert into a loud tool-result error — the
// Send never dispatched, so no result would ever arrive (design §3.2, the
// dropped-Send path; snapshot lookup so a mid-round re-add cannot silently
// drop the call).
func TestStaleFanoutErrorsLoudly(t *testing.T) {
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("x"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"messages": []messages.Message{messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "t9", Name: "Task", Args: map[string]any{"description": "ghost"}},
		}}},
		decisionsChannelName: decisionsState{PerCall: map[string]callDecision{
			"t9": {Run: true, Fanout: "subagent:ghost-0000"},
		}},
	}
	update, err := a.toolsNode()(lgruntime.NewRuntime(t.Context()), state)
	if err != nil {
		t.Fatal(err)
	}
	msgs := MessagesFrom(map[string]any{"messages": update.(map[string]any)["messages"]})
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "subagent:ghost-0000") {
		t.Fatalf("stale fanout must error naming the node; got %+v", msgs)
	}
}

// TestReproR1RelayChildSpeculatedSiblingRunsOnce: the archived relay defect
// R1, revived on the fan-out topology — a child batch [speculated read-only
// Bash probe, gated Write] must execute the probe EXACTLY once across the
// approval pause (the relay rebuilt the child and re-ran it; the subgraph
// child persists across its own pause).
func TestReproR1RelayChildSpeculatedSiblingRunsOnce(t *testing.T) {
	workdir := t.TempDir()
	exec := &relayProbeExec{Executor: executor.NewLocal()}
	a, err := New(Config{
		Model: &taskCallModel{args: taskArgs{Description: "probe", Prompt: "run the probe"}},
		SubagentModel: &subReportModel{batch: messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "s1", Name: "Bash", Args: map[string]any{"command": "echo " + relaySiblingMarker}},
			{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "gated.txt", "content": "x"}},
		}}},
		Registry:       tools.NewRegistry(workdir),
		Exec:           exec,
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-r1-subgraph"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("do it")}, thread))
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want 1 (the child's Write ask); got %#v", len(intrs), DecodeInterruptValue(intrs[0].Value))
	}
	waitProbeDone(t, exec, 1)
	intrs2, finalMsgs := pumpStream(t, a.ResumeStream(ctx, thread, ApprovalAnswer{Decisions: map[string]AskDecision{
		intrAskCallID(t, intrs[0]): {Outcome: OutcomeAllow},
	}}))
	if len(intrs2) != 0 {
		t.Fatalf("re-paused: %#v", DecodeInterruptValue(intrs2[0].Value))
	}
	// The child's report must surface the probe result exactly once and the
	// probe must have executed exactly once.
	res, ok := taskResultFor(finalMsgs, "t0")
	if !ok || !strings.Contains(res.Content, relaySiblingMarker) {
		t.Fatalf("child report lacks the probe result: %v", finalMsgs)
	}
	after, killed := exec.stats()
	if after != 1 || killed != 0 {
		t.Fatalf("probe executed %d times (killed %d), want exactly 1", after, killed)
	}
	if _, err := os.Stat(filepath.Join(workdir, "gated.txt")); err != nil {
		t.Fatalf("approved Write did not execute: %v", err)
	}
}

// intrAskCallID extracts the single ask's call id from a paused child
// ApprovalRequest.
func intrAskCallID(t *testing.T, in lgtypes.Interrupt) string {
	t.Helper()
	req, ok := DecodeInterruptValue(in.Value).(ApprovalRequest)
	if !ok || len(req.Asks) != 1 {
		t.Fatalf("pause payload = %#v, want a one-ask ApprovalRequest", DecodeInterruptValue(in.Value))
	}
	return req.Asks[0].CallID
}

// dualRoleModel serves both graph roles by prompt marker: the parent turn
// ("go") fans one Task out; every other prompt (the child's) answers with
// the instance's tag. Swapping instances via SetModel must reach the CHILD
// registry, not just the parent binding.
type dualRoleModel struct{ tag string }

func (m *dualRoleModel) pick(input []messages.Message) messages.Message {
	lastHuman := ""
	for _, msg := range input {
		if msg.Role == messages.RoleHuman {
			lastHuman = msg.Content
		}
		if msg.Role == messages.RoleTool {
			return messages.AI(m.tag + " report")
		}
	}
	if lastHuman == "go" {
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "t0", Name: "Task", Args: map[string]any{"description": "job"}},
		}}
	}
	return messages.AI(m.tag + " report")
}

func (m *dualRoleModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *dualRoleModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *dualRoleModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *dualRoleModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *dualRoleModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *dualRoleModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *dualRoleModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestSetModelRefreshesSubagentChildren: SetModel (the settings hot-swap
// entry) must refresh the subagent children too — the registry captures
// cfg.Model at child build when SubagentModel is unset, and a live provider
// switch that only rebinds the parent leaves every fan-out child on the
// dead old provider (the e2e debug run caught exactly that: parent on the
// scripted stand-in, children 401ing against the default provider).
func TestSetModelRefreshesSubagentChildren(t *testing.T) {
	a, err := New(Config{
		Model:          &dualRoleModel{tag: "old"},
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetModel(&dualRoleModel{tag: "new"}); err != nil {
		t.Fatal(err)
	}
	final := runTaskViaGraph(t, a, "thread-setmodel-children")
	res, ok := taskResultFor(final, "t0")
	if !ok || !strings.Contains(res.Content, "new report") {
		t.Fatalf("child kept the pre-SetModel model; report = %q (transcript %v)", res.Content, final)
	}
}
