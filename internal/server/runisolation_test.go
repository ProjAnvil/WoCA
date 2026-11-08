package server

// Run-isolation regression tests for speculative tool execution (spec
// decisions F1/F2): the agent's speculative registry is keyed by
// (run id, call id) and its futures live on the run context, so concurrent
// runs on one shared agent cannot poison or block each other through
// reused tool-call ids, and a run paused at a permission interrupt keeps
// its own speculated results for the resume.
//
// The scenarios mirror the Playwright e2e against real `woca serve`, where
// the scripted fake model is a shared OpenAI-protocol stand-in that numbers
// tool calls per-CONVERSATION: "call_1", "call_2", ... — so two sessions
// legitimately reuse the same call IDs:
//   - session A's model calls a permission-gated tool (Bash non-read-only)
//     in default permission mode -> the run PARKS awaiting POST /answer;
//   - while A is parked, session B runs a scripted tool-call turn
//     (Bash "ls" — read-only, auto-approved — then a plain answer);
//   - correct behavior: B completes with its own tool result.
//
// Before per-run keying, B either consumed A's cancelled future (result
// loss), blocked forever on A's still-running future (stall), or — single
// session — A's resume consumed its own future that the paused stream's
// context teardown had killed ("Exit code -1" instead of the real output).

import (
	"bufio"
	"context"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func bash(id, cmd string) messages.ToolCall {
	return messages.ToolCall{ID: id, Name: "Bash", Args: map[string]any{"command": cmd}}
}

func aiCalls(calls ...messages.ToolCall) messages.Message {
	return messages.Message{Role: messages.RoleAI, ToolCalls: calls}
}

// runIsolationScenario scripts the SHARED FakeChatModel. Response order is
// the global model-call order: A's first call parks A (its batch ends in a
// gated call), then B takes the next two (one tool round + a plain answer).
type runIsolationScenario struct {
	name  string
	aTurn messages.Message // A's batch: must end parked on a permission prompt
	bTurn messages.Message // B's tool round (auto-approved in default mode)
	bWant string           // substring B's tool_result must contain
}

var runIsolationScenarios = []runIsolationScenario{
	{
		// Control: provider-unique IDs (what Anthropic/OpenAI actually emit).
		name:  "control-distinct-ids",
		aTurn: aiCalls(bash("a1", "echo A-SECRET"), bash("a2", "touch gated-a")),
		bTurn: aiCalls(bash("b1", "echo B-OWN")),
		bWant: "B-OWN",
	},
	{
		// Same IDs, but A's batch is a single gated call: nothing of A's is
		// retained agent-globally (gated calls are neither speculated nor
		// memoized), so no collision.
		name:  "collide-single-gated",
		aTurn: aiCalls(bash("call_1", "touch gated-a")),
		bTurn: aiCalls(bash("call_1", "echo B-OWN")),
		bWant: "B-OWN",
	},
	{
		// e2e shape: per-conversation call numbering ("call_1", "call_2").
		// A's read-only sibling is speculatively executed and its future is
		// RETAINED across A's pause (same-process resume reuse); B's call_1
		// must resolve B's OWN execution, never A's future (cross-run
		// poisoning -> wrong/lost result).
		name:  "collide-spec-sibling-LOSS",
		aTurn: aiCalls(bash("call_1", "echo A-SECRET"), bash("call_2", "touch gated-a")),
		bTurn: aiCalls(bash("call_1", "echo B-OWN")),
		bWant: "B-OWN",
	},
	{
		// Same, with A's speculated sibling still RUNNING when A parks
		// (a read-only command that never exits on its own): B must not
		// block on A's future (cross-run wait stall).
		name:  "collide-spec-sibling-STALL",
		aTurn: aiCalls(bash("call_1", "tail -f /dev/null"), bash("call_2", "touch gated-a")),
		bTurn: aiCalls(bash("call_1", "echo B-OWN")),
		bWant: "B-OWN",
	},
}

// TestRunIsolation drives both sessions against one shared agent/server.
func TestRunIsolation(t *testing.T) {
	for _, sc := range runIsolationScenarios {
		for _, cpMode := range []string{"memory", "sqlite"} {
			t.Run(sc.name+"/"+cpMode, func(t *testing.T) { runIsolationPair(t, sc, cpMode) })
		}
	}
}

func runIsolationPair(t *testing.T, sc runIsolationScenario, cpMode string) {
	workdir := t.TempDir()
	cfg := agent.Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(sc.aTurn, sc.bTurn, messages.AI("B final answer")),
		),
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	}
	if cpMode == "sqlite" {
		cfg.CheckpointDir = t.TempDir()
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()})

	// --- Session A: drive to a parked permission interrupt. ---
	aStart := time.Now()
	runA, _ := createRun(t, ts.URL, "")
	parked := make(chan string, 1)
	go func() {
		for ev := range sseEvents(t, ts.URL, runA, 60*time.Second) {
			if ev.Type == "interrupt" {
				parked <- ev.Data
				return
			}
			if ev.Type == "done" || ev.Type == "error" {
				parked <- ("terminal-before-interrupt " + ev.Type + " " + ev.Data)
				return
			}
		}
	}()
	select {
	case d := <-parked:
		if !strings.Contains(d, "permission") && !strings.Contains(d, "terminal-before-interrupt") {
			t.Fatalf("A parked on unexpected interrupt: %s", d)
		}
		t.Logf("A parked after %.3fs: %s", time.Since(aStart).Seconds(), truncate(d, 220))
	case <-time.After(15 * time.Second):
		t.Fatal("A never parked on a permission interrupt")
	}
	t.Logf("A is parked; leaving it unanswered")

	// --- Session B: plain tool-call turn on a DIFFERENT session. ---
	bStart := time.Now()
	runB, _ := createRun(t, ts.URL, "")

	type bev struct {
		Type string
		Data string
		At   time.Duration
	}
	var mu sync.Mutex
	var events []bev
	done := make(chan string, 1)
	go func() {
		for ev := range sseEvents(t, ts.URL, runB, 60*time.Second) {
			mu.Lock()
			events = append(events, bev{ev.Type, truncate(ev.Data, 220), time.Since(bStart)})
			mu.Unlock()
			switch ev.Type {
			case "done":
				done <- "done"
				return
			case "error":
				done <- ("error: " + ev.Data)
				return
			}
		}
		done <- "sse-closed"
	}()

	select {
	case res := <-done:
		mu.Lock()
		evs := append([]bev(nil), events...)
		mu.Unlock()
		for _, e := range evs {
			t.Logf("B +%.4fs %-14s %s", e.At.Seconds(), e.Type, e.Data)
		}
		if res != "done" {
			t.Errorf("B did not complete cleanly: %s", res)
		}
		var toolResult string
		for _, e := range evs {
			if e.Type == "tool_result" {
				toolResult = e.Data
			}
		}
		switch {
		case toolResult == "":
			t.Errorf("B LOST its tool result: no tool_result event before %q", res)
		case !strings.Contains(toolResult, sc.bWant):
			t.Errorf("B got the WRONG tool result: %s (want substring %q)", toolResult, sc.bWant)
		}
	case <-time.After(20 * time.Second):
		mu.Lock()
		for _, e := range events {
			t.Logf("B +%.4fs %-14s %s (STALLED)", e.At.Seconds(), e.Type, e.Data)
		}
		mu.Unlock()
		t.Errorf("B STALLED: no terminal event within 20s (%.3fs elapsed); interesting goroutine stacks follow", time.Since(bStart).Seconds())
		dumpInterestingStacks(t)
	}

	// Cancel A so the test server can shut down cleanly (this also releases
	// A's still-running speculative sibling in the STALL scenario).
	_ = postJSON(t, ts.URL+"/api/runs/"+runA+"/cancel", `{}`)
}

// TestRunIsolationSelfLossOnResume: single session, no concurrency. A's
// read-only sibling is speculated during model streaming; the gated sibling
// parks A. The retained future is started on the RUN context, which stays
// alive across the pause (park blocks, no cancel) — so on resume A consumes
// its own real result. Before the fix the future inherited the paused
// stream's context, which the stream iterator teardown cancelled, and the
// resume consumed the cancellation artifact as A's tool result.
func TestRunIsolationSelfLossOnResume(t *testing.T) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(
				aiCalls(bash("a1", "echo A-OWN"), bash("a2", "touch gated-a")),
				messages.AI("A final answer"),
			),
		),
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	runA, _ := createRun(t, ts.URL, "")
	results := map[string]string{}
	for ev := range sseEvents(t, ts.URL, runA, 20*time.Second) {
		t.Logf("A %-14s %s", ev.Type, truncate(ev.Data, 160))
		switch ev.Type {
		case "interrupt":
			if err := postJSON(t, ts.URL+"/api/runs/"+runA+"/answer", `{"approve":true}`); err != nil {
				t.Fatal(err)
			}
		case "tool_result":
			for _, id := range []string{"a1", "a2"} {
				if strings.Contains(ev.Data, `"tool_call_id":"`+id+`"`) {
					results[id] = ev.Data
				}
			}
		}
		if ev.Type == "done" || ev.Type == "error" {
			break
		}
	}
	if !strings.Contains(results["a1"], "A-OWN") {
		t.Errorf("A LOST its own speculated sibling result on resume: %q (want A-OWN)", results["a1"])
	}
}

// sseEvents subscribes to a run's SSE stream and yields parsed events until
// the ctx deadline or stream end.
func sseEvents(t *testing.T, base, runID string, timeout time.Duration) <-chan struct {
	Type string
	Data string
} {
	t.Helper()
	out := make(chan struct {
		Type string
		Data string
	}, 64)
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
			out <- ev
		}
	}()
	return out
}

// dumpInterestingStacks logs the woca/langchain frames of every goroutine —
// the stall diagnostics for the STALL scenario failure path.
func dumpInterestingStacks(t *testing.T) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "projanvil/woca") || strings.Contains(g, "projanvil/langchain") {
			lines := strings.Split(g, "\n")
			var b strings.Builder
			b.WriteString(lines[0] + "\n")
			for _, l := range lines[1:] {
				if strings.Contains(l, "woca/") || strings.Contains(l, "langchain") {
					b.WriteString(l + "\n")
				}
			}
			t.Logf("STACK>\n%s", b.String())
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
