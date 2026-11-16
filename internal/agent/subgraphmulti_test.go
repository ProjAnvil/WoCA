package agent

// Task 8 (graph-native HITL): multi-interrupt pauses. A superstep's pause
// carries EVERY interrupt of the superstep (N parallel children → N cards,
// each addressed by its fork NS); answers are NS-keyed maps with fork-native
// partial semantics (unanswered children re-pause with only the remainder),
// durable across processes, and the goal relay forwards the whole list.
// X2's superstep barrier is pinned here as documentation: a sibling's
// approval surfaces only after the slowest task of the superstep settles.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/cpserde"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/tools"
)

// fanOutTasksModel is the parameterized parent: one foreground Task call per
// entry (call id → child prompt), re-issued until every call id has a tool
// result, then "parent done". Transcript-driven like n1ParentModel (a
// cursor-scripted fake would replay its batch on resume).
type fanOutTasksModel struct{ prompts map[string]string }

func (m *fanOutTasksModel) pick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	calls := make([]messages.ToolCall, 0, len(m.prompts))
	done := true
	for id, prompt := range m.prompts {
		if !answered[id] {
			done = false
		}
		calls = append(calls, messages.ToolCall{ID: id, Name: "Task", Args: map[string]any{
			"description": id, "prompt": prompt,
		}})
	}
	if done {
		return messages.AI("parent done")
	}
	slices.SortFunc(calls, func(a, b messages.ToolCall) int { return strings.Compare(a.ID, b.ID) })
	return messages.Message{Role: messages.RoleAI, ToolCalls: calls}
}

func (m *fanOutTasksModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *fanOutTasksModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *fanOutTasksModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *fanOutTasksModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *fanOutTasksModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *fanOutTasksModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *fanOutTasksModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// pauseByFile maps each pause interrupt to the file its child wants written
// (the promptFileModel children ask Write for a.txt/b.txt).
func pauseByFile(t *testing.T, intrs []lgtypes.Interrupt) map[string]string {
	t.Helper()
	byFile := map[string]string{}
	for _, intr := range intrs {
		req, ok := DecodeInterruptValue(intr.Value).(ApprovalRequest)
		if !ok || len(req.Asks) != 1 {
			t.Fatalf("pause payload = %#v, want a one-ask ApprovalRequest", DecodeInterruptValue(intr.Value))
		}
		file, _ := req.Asks[0].ToolArgs["file_path"].(string)
		byFile[file] = intr.NS
	}
	return byFile
}

// TestPartialMapAnswerRepausesRemainder: two children pause in one
// superstep; answering ONLY one child's NS resumes that child and re-pauses
// with EXACTLY the other child's interrupt (fork-native partial-map
// semantics, design §3.4).
func TestPartialMapAnswerRepausesRemainder(t *testing.T) {
	workdir := t.TempDir()
	a, err := New(Config{
		Model:          &fanOutTasksModel{prompts: map[string]string{"tA": "FILE-A write it", "tB": "FILE-B write it"}},
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
	thread := "thread-partial-map"
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("go")}, thread))
	if len(intrs) != 2 {
		t.Fatalf("pauses = %d, want 2 (both children's Write asks); first = %#v", len(intrs), DecodeInterruptValue(intrs[0].Value))
	}
	byFile := pauseByFile(t, intrs)
	allowNS, otherNS := byFile["a.txt"], byFile["b.txt"]
	if allowNS == "" || otherNS == "" || allowNS == otherNS {
		t.Fatalf("interrupt NSes = %q / %q, want both present and distinct", allowNS, otherNS)
	}
	// Answer ONLY the a.txt child (partial map).
	partial := map[string]any{}
	for _, intr := range intrs {
		if intr.NS != allowNS {
			continue
		}
		partial[allowNS] = ApprovalAnswer{Decisions: map[string]AskDecision{
			intrAskCallID(t, intr): {Outcome: OutcomeAllow},
		}}
	}
	intrs2, _ := pumpStream(t, a.ResumeStream(ctx, thread, partial))
	if len(intrs2) != 1 {
		t.Fatalf("re-pause carries %d interrupts, want exactly 1 (the unanswered child); got %#v", len(intrs2), DecodeInterruptValue(intrs2[0].Value))
	}
	if intrs2[0].NS != otherNS {
		t.Fatalf("re-paused interrupt NS = %q, want the unanswered child's %q", intrs2[0].NS, otherNS)
	}
	req := DecodeInterruptValue(intrs2[0].Value).(ApprovalRequest)
	if file, _ := req.Asks[0].ToolArgs["file_path"].(string); file != "b.txt" {
		t.Fatalf("re-paused ask = %#v, want the b.txt Write", req.Asks[0])
	}
	// Answer the remainder with a scalar deny (single pending → scalar resume).
	intrs3, final := pumpStream(t, a.ResumeStream(ctx, thread, ApprovalAnswer{Decisions: map[string]AskDecision{
		intrAskCallID(t, intrs2[0]): {Outcome: OutcomeDeny},
	}}))
	if len(intrs3) != 0 {
		t.Fatalf("re-paused again: %#v", DecodeInterruptValue(intrs3[0].Value))
	}
	if _, err := os.Stat(filepath.Join(workdir, "a.txt")); err != nil {
		t.Fatal("answered child's Write did not execute")
	}
	if _, err := os.Stat(filepath.Join(workdir, "b.txt")); err == nil {
		t.Fatal("unanswered-then-denied child's Write executed")
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

// cpChildModel drives the cross-process scenario's children, keyed by the
// job marker in the child prompt: A and B issue gated Bash appends (they
// pause on the ask), C issues a read-only probe and completes.
type cpChildModel struct{ ranLog string }

func (m *cpChildModel) pick(input []messages.Message) messages.Message {
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
	}
	prompt := ""
	for _, msg := range input {
		if msg.Role == messages.RoleHuman && prompt == "" {
			prompt = msg.Content
		}
	}
	switch {
	case strings.Contains(prompt, "JOB-A"):
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "ba1", Name: "Bash", Args: map[string]any{"command": "echo run-A >> " + m.ranLog}},
		}}
	case strings.Contains(prompt, "JOB-B"):
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "bb1", Name: "Bash", Args: map[string]any{"command": "echo run-B >> " + m.ranLog}},
		}}
	case strings.Contains(prompt, "JOB-C"):
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "bc1", Name: "Bash", Args: map[string]any{"command": "echo probe-C"}},
		}}
	}
	return messages.AI("sub done")
}

func (m *cpChildModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *cpChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *cpChildModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *cpChildModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *cpChildModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *cpChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *cpChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// fileLogExec records every executed bash command line into a log file —
// the cross-process execution counter (both processes append to the same
// file; the count of a marker line is how many times that command ran
// across processes).
type fileLogExec struct {
	executor.Executor
	path string
	mu   sync.Mutex
}

func (e *fileLogExec) Exec(ctx context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	res, err := e.Executor.Exec(ctx, req)
	if req.Name == "bash" && len(req.Args) == 2 {
		e.mu.Lock()
		if f, oerr := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); oerr == nil {
			_, _ = f.WriteString(req.Args[1] + "\n")
			_ = f.Close()
		}
		e.mu.Unlock()
	}
	return res, err
}

func countLogLines(t *testing.T, path, marker string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, marker) {
			n++
		}
	}
	return n
}

// TestCrossInstanceSubgraphResume (a fair cross-process simulation: a fresh
// Agent with its own saver handle over the same sqlite file; same binary,
// so OS-level serialization is exercised only through the sqlite serde): agent A pauses on two gated children (plus
// a third child that completes inside the same superstep) over a durable
// sqlite saver; a NEW agent over the same database file resumes the pause
// with the NS-keyed map. Coverage the fork itself lacks: the completed
// sibling replays from its checkpoint writes (its probe never re-runs) and
// each gated child executes exactly once across the two processes.
func TestCrossInstanceSubgraphResume(t *testing.T) {
	workdir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "crossproc.db")
	ranLog := filepath.Join(workdir, "ran.log")
	execLog := filepath.Join(workdir, "exec.log")

	parent := &fanOutTasksModel{prompts: map[string]string{
		"tA": "JOB-A append the marker",
		"tB": "JOB-B append the marker",
		"tC": "JOB-C run the probe",
	}}
	child := &cpChildModel{ranLog: ranLog}

	// Process A: run to the pause, then tear down without answering.
	saverA, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	a, err := New(Config{
		Model:          parent,
		SubagentModel:  child,
		Registry:       tools.NewRegistry(workdir),
		Exec:           &fileLogExec{Executor: executor.NewLocal(), path: execLog},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
		Checkpointer:   saverA,
	})
	if err != nil {
		t.Fatal(err)
	}
	thread := "thread-cross-proc"
	ctx := t.Context()
	intrs, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("go")}, thread))
	if len(intrs) != 2 {
		t.Fatalf("pauses = %d, want 2 (the two gated children); got %#v", len(intrs), DecodeInterruptValue(intrs[0].Value))
	}
	byCmd := map[string]string{} // marker in the gated command -> interrupt NS
	for _, intr := range intrs {
		req, ok := DecodeInterruptValue(intr.Value).(ApprovalRequest)
		if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Bash" {
			t.Fatalf("pause payload = %#v, want a one-ask Bash ApprovalRequest", DecodeInterruptValue(intr.Value))
		}
		cmd, _ := req.Asks[0].ToolArgs["command"].(string)
		switch {
		case strings.Contains(cmd, "run-A"):
			byCmd["run-A"] = intr.NS
		case strings.Contains(cmd, "run-B"):
			byCmd["run-B"] = intr.NS
		default:
			t.Fatalf("unexpected gated command %q", cmd)
		}
	}
	// The completing sibling's read-only probe ran exactly once in process A
	// (the superstep barrier means it settled before the pause surfaced).
	if n := countLogLines(t, execLog, "echo probe-C"); n != 1 {
		t.Fatalf("probe-C executions in process A = %d, want 1", n)
	}
	if err := saverA.Close(); err != nil {
		t.Fatalf("close saver A: %v", err)
	}

	// Process B: a fresh agent over the same sqlite file resumes the pause
	// with the map — allow run-A, deny run-B.
	saverB, err := lgcpsqlite.New(dbPath, cpserde.NewSerializer())
	if err != nil {
		t.Fatalf("sqlite.New (B): %v", err)
	}
	defer func() { _ = saverB.Close() }()
	b, err := New(Config{
		Model:          parent,
		SubagentModel:  child,
		Registry:       tools.NewRegistry(workdir),
		Exec:           &fileLogExec{Executor: executor.NewLocal(), path: execLog},
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
		Checkpointer:   saverB,
	})
	if err != nil {
		t.Fatal(err)
	}
	resume := map[string]any{
		byCmd["run-A"]: ApprovalAnswer{Decisions: map[string]AskDecision{"ba1": {Outcome: OutcomeAllow}}},
		byCmd["run-B"]: ApprovalAnswer{Decisions: map[string]AskDecision{"bb1": {Outcome: OutcomeDeny}}},
	}
	intrs2, final := pumpStream(t, b.ResumeStream(ctx, thread, resume))
	if len(intrs2) != 0 {
		t.Fatalf("re-paused after the map resume: %#v", DecodeInterruptValue(intrs2[0].Value))
	}
	// Exactly-once across processes: the completed sibling replayed (probe
	// still 1), the allowed gated call ran once, the denied one never ran.
	if n := countLogLines(t, execLog, "echo probe-C"); n != 1 {
		t.Fatalf("probe-C executions across processes = %d, want 1 (completed sibling must replay, not re-run)", n)
	}
	if n := countLogLines(t, execLog, "echo run-A"); n != 1 {
		t.Fatalf("run-A executions across processes = %d, want 1", n)
	}
	if n := countLogLines(t, execLog, "echo run-B"); n != 0 {
		t.Fatalf("run-B executions across processes = %d, want 0 (denied)", n)
	}
	if n := countLogLines(t, ranLog, "run-A"); n != 1 {
		t.Fatalf("run-A side effect = %d, want 1", n)
	}
	if n := countLogLines(t, ranLog, "run-B"); n != 0 {
		t.Fatalf("run-B side effect = %d, want 0", n)
	}
	for _, id := range []string{"tA", "tB", "tC"} {
		if _, ok := taskResultFor(final, id); !ok {
			t.Fatalf("task %s report missing: %v", id, final)
		}
	}
	if got := final[len(final)-1].Content; got != "parent done" {
		t.Fatalf("parent did not complete; last = %q", got)
	}
}

// barrierChildModel: the FILE-A child asks its Write immediately; the SLOW
// child sleeps `delay` before its read-only probe round, then completes.
type barrierChildModel struct{ delay time.Duration }

func (m *barrierChildModel) pick(input []messages.Message) messages.Message {
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
	}
	prompt := ""
	for _, msg := range input {
		if msg.Role == messages.RoleHuman && prompt == "" {
			prompt = msg.Content
		}
	}
	if strings.Contains(prompt, "SLOW") {
		time.Sleep(m.delay)
		return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "bs1", Name: "Bash", Args: map[string]any{"command": "echo slow-probe-settled"}},
		}}
	}
	return writeCall("w-a.txt", "a.txt", "x")
}

func (m *barrierChildModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *barrierChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *barrierChildModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *barrierChildModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *barrierChildModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *barrierChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *barrierChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// slowProbeExec stamps the completion time of the slow sibling's probe.
type slowProbeExec struct {
	executor.Executor
	mu     sync.Mutex
	doneAt time.Time
}

func (e *slowProbeExec) Exec(ctx context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	res, err := e.Executor.Exec(ctx, req)
	if req.Name == "bash" && len(req.Args) == 2 && strings.Contains(req.Args[1], "slow-probe-settled") {
		e.mu.Lock()
		e.doneAt = time.Now()
		e.mu.Unlock()
	}
	return res, err
}

func (e *slowProbeExec) settledAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.doneAt
}

// TestSubgraphSuperstepBarrierDocumentsX2 pins the documented X2 barrier:
// interrupts aggregate per superstep, so one child's approval surfaces only
// after the SLOWEST sibling of the superstep settles — the fast child's ask
// (raised immediately) is not observable until the ~300ms sibling finishes.
func TestSubgraphSuperstepBarrierDocumentsX2(t *testing.T) {
	workdir := t.TempDir()
	exec := &slowProbeExec{Executor: executor.NewLocal()}
	const siblingDelay = 300 * time.Millisecond
	a, err := New(Config{
		Model: &fanOutTasksModel{prompts: map[string]string{
			"tFast": "FILE-A write it",
			"tSlow": "SLOW take your time",
		}},
		SubagentModel:  &barrierChildModel{delay: siblingDelay},
		Registry:       tools.NewRegistry(workdir),
		Exec:           exec,
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	intrs, _ := pumpStream(t, a.Stream(t.Context(), []messages.Message{messages.Human("go")}, "thread-x2-barrier"))
	pauseAt := time.Now()
	settled := exec.settledAt()
	if settled.IsZero() {
		t.Fatal("slow sibling's probe never executed before the pause")
	}
	if len(intrs) != 1 {
		t.Fatalf("pauses = %d, want exactly 1 (only the fast child asks; the slow child completed)", len(intrs))
	}
	if req, ok := DecodeInterruptValue(intrs[0].Value).(ApprovalRequest); !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
		t.Fatalf("pause payload = %#v, want the fast child's Write ask", DecodeInterruptValue(intrs[0].Value))
	}
	// The barrier: the pause is observed only after the slow sibling settled,
	// and no earlier than its configured delay.
	if pauseAt.Before(settled) {
		t.Fatalf("pause observed at %v, before the slow sibling settled at %v — interrupts must aggregate per superstep", pauseAt, settled)
	}
	if elapsed := pauseAt.Sub(start); elapsed < siblingDelay-50*time.Millisecond {
		t.Fatalf("pause surfaced after %v, want >= ~%v (the superstep barrier)", elapsed, siblingDelay)
	}
}

// goalFanoutModel scripts the goal loop with a fan-out executor: assess and
// evaluate answer their focused prompts; execute rounds issue the two Task
// calls until both reports are back.
type goalFanoutModel struct{}

func (m *goalFanoutModel) pick(input []messages.Message) messages.Message {
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
	if answered["tA"] && answered["tB"] {
		return messages.AI("all done")
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "tA", Name: "Task", Args: map[string]any{"description": "a", "prompt": "FILE-A write it"}},
		{ID: "tB", Name: "Task", Args: map[string]any{"description": "b", "prompt": "FILE-B write it"}},
	}}
}

func (m *goalFanoutModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *goalFanoutModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *goalFanoutModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *goalFanoutModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}
func (m *goalFanoutModel) OutputSchema() schema.Schema                            { return schema.Schema{"type": "object"} }
func (m *goalFanoutModel) BindTools([]coretools.Tool) (language.ChatModel, error) { return m, nil }
func (m *goalFanoutModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestGoalMultiInterruptMapResume (M2): the goal execute cycle's inner agent
// fans out two Tasks that both pause; the goal relay forwards the WHOLE
// interrupt list as one aggregate, and GoalResume answers both children
// through the NS-keyed map (wrapped for the outer goal interrupt).
func TestGoalMultiInterruptMapResume(t *testing.T) {
	workdir := t.TempDir()
	a, err := New(Config{
		Model:          &goalFanoutModel{},
		SubagentModel:  &promptFileModel{},
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
	stream, err := a.GoalStream(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := pumpGoalChunks(t, stream)
	if len(res.interrupts) != 1 {
		t.Fatalf("goal pauses = %d, want 1 aggregate; err=%v", len(res.interrupts), res.err)
	}
	outer := res.interrupts[0]
	agg, ok := outer.Value.(MultiInterrupt)
	if !ok {
		t.Fatalf("goal pause payload = %T, want MultiInterrupt carrying the inner pause's whole list", outer.Value)
	}
	if len(agg.Interrupts) != 2 {
		t.Fatalf("aggregate carries %d interrupts, want 2 (both children)", len(agg.Interrupts))
	}
	inner := map[string]any{}
	for _, card := range agg.Interrupts {
		req, ok := card.Value.(ApprovalRequest)
		if !ok || len(req.Asks) != 1 || req.Asks[0].ToolName != "Write" {
			t.Fatalf("aggregate card = %#v, want a one-ask Write ApprovalRequest", card.Value)
		}
		file, _ := req.Asks[0].ToolArgs["file_path"].(string)
		outcome := OutcomeDeny
		if file == "a.txt" {
			outcome = OutcomeAllow
		}
		inner[card.NS] = ApprovalAnswer{Decisions: map[string]AskDecision{
			req.Asks[0].CallID: {Outcome: outcome},
		}}
	}
	// The outer goal interrupt is addressed by its own key; the value it
	// receives is the inner graph's NS-keyed map.
	outerKey := outer.NS
	if outerKey == "" {
		outerKey = outer.ID
	}
	resumed, err := a.GoalResume(t.Context(), map[string]any{outerKey: inner})
	if err != nil {
		t.Fatal(err)
	}
	res2 := pumpGoalChunks(t, resumed)
	if res2.err != nil || len(res2.interrupts) > 0 {
		t.Fatalf("goal resume failed: %v (interrupts: %#v)", res2.err, res2.interrupts)
	}
	if g := a.Goal(); g.Status != GoalComplete {
		t.Fatalf("goal status = %q, want complete", g.Status)
	}
	if _, err := os.Stat(filepath.Join(workdir, "a.txt")); err != nil {
		t.Fatal("allowed child's file missing after goal resume")
	}
	if _, err := os.Stat(filepath.Join(workdir, "b.txt")); err == nil {
		t.Fatal("denied child's file exists after goal resume")
	}
}
