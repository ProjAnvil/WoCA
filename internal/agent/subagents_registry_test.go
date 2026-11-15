package agent

// Task 6: the content-addressed subagent child registry and the graph
// rebuild it drives (graph-native HITL design §3.2). Node names embed a
// fingerprint of the def's content, so same-name defs from different
// workspaces stay distinct, def edits mint a new node, and a paused run
// whose plan references a node that no longer exists refuses to resume.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newRegistryAgent builds a minimal agent with the given per-workspace def
// resolver wired (nil keeps no resolver: defs resolve through the startup
// set only).
func newRegistryAgent(t *testing.T, agentsFor func(workdir string) []agents.Definition) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(messages.AI("ok")),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 20,
		AgentsFor:      agentsFor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var hex4 = regexp.MustCompile(`^[0-9a-f]{4}$`)

func TestDefFingerprintStableAndSensitive(t *testing.T) {
	base := agents.Definition{Name: "reviewer", Prompt: "You review code.", Tools: []string{"Read", "Grep", "Bash"}}
	// A separately constructed def with the same content (tools in a
	// different order) must fingerprint identically: the hash is pure, so
	// it is stable across rebuilds and across processes.
	same := agents.Definition{Name: "reviewer", Prompt: "You review code.", Tools: []string{"Grep", "Bash", "Read"}}
	fp := defFingerprint(base)
	if fp == "" || !hex4.MatchString(fp) {
		t.Fatalf("fingerprint %q is not 4 lowercase hex chars", fp)
	}
	if got := defFingerprint(same); got != fp {
		t.Fatalf("fingerprint not stable / tool-order sensitive: %q vs %q", fp, got)
	}
	// Same def hashed twice (a rebuild) is identical.
	if got := defFingerprint(base); got != fp {
		t.Fatalf("fingerprint not deterministic: %q vs %q", fp, got)
	}
	edited := base
	edited.Prompt += " Be strict."
	if got := defFingerprint(edited); got == fp {
		t.Fatal("prompt edit must change the fingerprint")
	}
	renamed := base
	renamed.Name = "auditor"
	if got := defFingerprint(renamed); got == fp {
		t.Fatal("name edit must change the fingerprint")
	}
	retooled := base
	retooled.Tools = []string{"Read"}
	if got := defFingerprint(retooled); got == fp {
		t.Fatal("tool list edit must change the fingerprint")
	}
}

func TestSubagentNodeNameSanitizes(t *testing.T) {
	def := agents.Definition{Name: "data miner!!", Prompt: "p"}
	node := subagentNodeName(def)
	want := subagentNodePrefix + "data-miner--" + "-" + defFingerprint(def)
	if node != want {
		t.Fatalf("node = %q, want %q", node, want)
	}
	// The sanitized segment keeps only [a-zA-Z0-9-_].
	segment := strings.TrimSuffix(strings.TrimPrefix(node, subagentNodePrefix), "-"+defFingerprint(def))
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			t.Fatalf("node %q carries unsanitized rune %q", node, r)
		}
	}
	// Non-ASCII collapses too.
	uni := agents.Definition{Name: "résumé", Prompt: "p"}
	if got := subagentNodeName(uni); !strings.HasPrefix(got, subagentNodePrefix+"r-sum-") {
		t.Fatalf("unicode name node = %q", got)
	}
	// Same name, different content → distinct node names (the §3.2 rule).
	other := def
	other.Prompt = "q"
	if subagentNodeName(def) == subagentNodeName(other) {
		t.Fatal("same-name/different-content defs must map to distinct nodes")
	}
}

func TestChildRegistryRegistersDistinctNodesPerContent(t *testing.T) {
	ws1, ws2 := t.TempDir(), t.TempDir()
	shared := agents.Definition{Name: "reviewer", Prompt: "You review code.", Tools: []string{"Read", "Grep"}}
	other := agents.Definition{Name: "reviewer", Prompt: "You review code differently.", Tools: []string{"Read"}}
	table := map[string][]agents.Definition{ws1: {shared}, ws2: {other}}
	a := newRegistryAgent(t, func(ws string) []agents.Definition { return table[ws] })

	a.agentsFor(WithRunWorkspace(t.Context(), ws1))
	a.agentsFor(WithRunWorkspace(t.Context(), ws2))
	a.flushGraphRebuild()

	n1, n2 := subagentNodeName(shared), subagentNodeName(other)
	if n1 == n2 {
		t.Fatalf("same-name defs with different content collided: %q", n1)
	}
	for _, tc := range []struct{ node, prompt string }{{n1, shared.Prompt}, {n2, other.Prompt}} {
		child := a.childReg().lookup(tc.node)
		if child == nil {
			t.Fatalf("node %q not registered (have %v)", tc.node, a.childReg().nodes())
		}
		if !child.subagent {
			t.Fatalf("child registered for %q is not a subagent", tc.node)
		}
		if got := child.SystemPrompt(); got != tc.prompt {
			t.Fatalf("child prompt for %q = %q, want the def prompt %q", tc.node, got, tc.prompt)
		}
	}
}

func TestRebuildDropsRemovedDefsKeepsOthersStable(t *testing.T) {
	ws := t.TempDir()
	defA1 := agents.Definition{Name: "alpha", Prompt: "v1"}
	defA2 := agents.Definition{Name: "alpha", Prompt: "v2"}
	defB := agents.Definition{Name: "beta", Prompt: "b"}
	current := []agents.Definition{defA1, defB}
	a := newRegistryAgent(t, func(string) []agents.Definition { return current })

	ctx := WithRunWorkspace(t.Context(), ws)
	a.agentsFor(ctx)
	a.flushGraphRebuild()

	nA1, nA2, nB := subagentNodeName(defA1), subagentNodeName(defA2), subagentNodeName(defB)
	if a.childReg().lookup(nA1) == nil || a.childReg().lookup(nB) == nil {
		t.Fatalf("initial registration missing nodes (have %v)", a.childReg().nodes())
	}

	// Def-set change: alpha edited, beta removed. The cache Set is the hook
	// point (a resolver hit would return the stale cached list).
	current = []agents.Definition{defA2}
	a.wsAgents.Set(ws, current)
	a.flushGraphRebuild()

	if a.childReg().lookup(nA1) != nil {
		t.Fatal("edited def's old node must disappear from the registry")
	}
	if a.childReg().lookup(nB) != nil {
		t.Fatal("removed def's node must disappear from the registry")
	}
	if a.childReg().lookup(nA2) == nil {
		t.Fatalf("edited def's new node missing (have %v)", a.childReg().nodes())
	}
	// The surviving node set is exactly the current def set.
	if got, want := strings.Join(a.childReg().nodes(), ","), nA2; got != want {
		t.Fatalf("node set after rebuild = %q, want %q", got, want)
	}
	// Repeated rebuilds keep node names stable (content addressing).
	a.flushGraphRebuild()
	if a.childReg().lookup(nA2) == nil || a.childReg().len() != 1 {
		t.Fatalf("rebuild not stable: %v", a.childReg().nodes())
	}
}

func TestWSAgentSetSchedulesDebouncedRebuild(t *testing.T) {
	ws := t.TempDir()
	def := agents.Definition{Name: "gamma", Prompt: "g"}
	current := []agents.Definition{def}
	a := newRegistryAgent(t, func(string) []agents.Definition { return current })

	a.wsAgents.Set(ws, current)
	node := subagentNodeName(def)
	// No explicit flush: the debounce timer (250ms) must land the node by
	// itself; generous deadline to absorb scheduler jitter.
	deadline := time.Now().Add(3 * time.Second)
	for a.childReg().lookup(node) == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.childReg().lookup(node) == nil {
		t.Fatalf("debounced rebuild never registered %q (have %v)", node, a.childReg().nodes())
	}
}

func TestZeroDefsGraphCompilesAndRuns(t *testing.T) {
	a := newRegistryAgent(t, nil)
	if got := a.childReg().len(); got != 0 {
		t.Fatalf("zero-def agent registered %d subagent nodes, want 0", got)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t-zero-defs")
	if err != nil {
		t.Fatalf("invoke on zero-def graph: %v", err)
	}
	if last := out[len(out)-1]; last.Role != messages.RoleAI || last.Content != "ok" {
		t.Fatalf("last message = %+v", last)
	}
}

func TestStartupAgentDefinitionsRegisterAtNew(t *testing.T) {
	def := agents.Definition{Name: "explorer", Prompt: "explore things"}
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		Agents:         []agents.Definition{def},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.childReg().lookup(subagentNodeName(def)) == nil {
		t.Fatalf("startup def not registered at New (have %v)", a.childReg().nodes())
	}
}

// pauseAtSubagentNode writes the checkpoint a paused run would have left
// behind had it planned work in the given subagent node: the resume
// machinery replans from Checkpoint.Next verbatim, so this is exactly the
// durable state a Task-7 Send pause persists.
func pauseAtSubagentNode(t *testing.T, a *Agent, thread, node string) {
	t.Helper()
	cp := lgcheckpoint.Checkpoint{
		V:               1,
		ID:              "cp-sub-" + thread,
		ChannelValues:   map[string]any{"messages": []messages.Message{messages.Human("run the reviewer")}},
		ChannelVersions: map[string]int64{"messages": 1},
		Next:            []lgcheckpoint.PlannedTask{{ID: "task-1", Node: node}},
	}
	if _, err := a.saver.Put(t.Context(), lgcheckpoint.Config{ThreadID: thread}, cp, lgcheckpoint.Metadata{Source: "loop", Step: 2}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAfterDefEditErrorsNamingDef(t *testing.T) {
	ws := t.TempDir()
	def := agents.Definition{Name: "reviewer", Prompt: "You review code."}
	edited := agents.Definition{Name: "reviewer", Prompt: "You review code. Now stricter."}
	current := []agents.Definition{def}
	a := newRegistryAgent(t, func(string) []agents.Definition { return current })

	ctx := WithRunWorkspace(context.Background(), ws)
	a.agentsFor(ctx)
	a.flushGraphRebuild()
	node := subagentNodeName(def)
	if a.childReg().lookup(node) == nil {
		t.Fatalf("def node %q not registered", node)
	}

	pauseAtSubagentNode(t, a, "t-edit", node)
	// Before the edit the resume pre-check passes (the node is live).
	if err := a.checkResumableSubagents(ctx, "t-edit"); err != nil {
		t.Fatalf("pre-edit check failed: %v", err)
	}

	// The def changes on disk while the run sits paused.
	current = []agents.Definition{edited}
	a.wsAgents.Set(ws, current)
	a.flushGraphRebuild()

	_, err := a.Resume(ctx, "t-edit", "y")
	if err == nil {
		t.Fatal("resume after def edit must fail")
	}
	if !strings.Contains(err.Error(), def.Name) {
		t.Fatalf("resume error must name the def %q: %v", def.Name, err)
	}
	if !strings.Contains(err.Error(), node) {
		t.Fatalf("resume error must name the missing node %q: %v", node, err)
	}

	// Same contract when the def disappears entirely.
	pauseAtSubagentNode(t, a, "t-removed", node)
	current = nil
	a.wsAgents.Set(ws, current)
	a.flushGraphRebuild()
	_, err = a.Resume(ctx, "t-removed", "y")
	if err == nil || !strings.Contains(err.Error(), def.Name) {
		t.Fatalf("resume after def removal must error naming %q, got %v", def.Name, err)
	}
}

// TestDefEditDoesNotBlockMainGraphResume is the negative control: a def
// edited under a pause whose plan references NO subagent node (an ordinary
// approval pause) resumes normally — the missing-node check must never
// over-fire on main-graph pauses.
func TestDefEditDoesNotBlockMainGraphResume(t *testing.T) {
	ws := t.TempDir()
	def := agents.Definition{Name: "reviewer", Prompt: "p1"}
	edited := agents.Definition{Name: "reviewer", Prompt: "p2"}
	current := []agents.Definition{def}
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "x.txt", "content": "hi"}},
			}},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 20,
		AgentsFor:      func(string) []agents.Definition { return current },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithRunWorkspace(t.Context(), ws)
	_, err = a.Invoke(ctx, []messages.Message{messages.Human("write it")}, "t-main")
	interruptFromError(t, err) // a real pause, at the approval superstep

	// The def changes on disk while the run sits paused.
	current = []agents.Definition{edited}
	a.wsAgents.Set(ws, current)
	a.flushGraphRebuild()

	out, err := a.Resume(ctx, "t-main", "y")
	if err != nil {
		t.Fatalf("main-graph resume must not be blocked by a def edit: %v", err)
	}
	if last := out[len(out)-1]; last.Role != messages.RoleAI || last.Content != "done" {
		t.Fatalf("resumed run final = %+v", last)
	}
}
