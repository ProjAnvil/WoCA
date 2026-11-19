package server

// run_cancelpartial_test.go pins the cancel-plane closure of the partial
// transcript (spec 2026-11-19-cancel-partial-output §D3/§D4): a cancelled
// executing run emits one synthesized tool_result event per dangling call
// BEFORE its terminal done frame, and persists the four-segment derived
// history (base + human? + partial + synth + stop notice).
//
// The parked-approval window is the deterministic dangling fixture: the
// model has committed its tool call, the approval gate parks before the
// tools superstep is ever dispatched, and the cancel aborts with the call
// unanswered. (A mid-tool cancel is a genuine race in the engine — the
// fork's stream emitter drops the tools batch on a cancelled ctx with
// select-random priority, so the call is answered by the tool's own
// cancelled output about half the time. Both outcomes are legal and both
// close the card; only the parked window pins the synth branch without
// flaking.)

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// awaitRunEvent polls the run's event ring until one event of the type exists.
func awaitRunEvent(t *testing.T, s *Server, runID, typ string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.lookupRun(runID); r != nil {
			r.mu.Lock()
			seen := false
			for _, ev := range r.history {
				if ev.Type == typ {
					seen = true
					break
				}
			}
			r.mu.Unlock()
			if seen {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never emitted a %s event", runID, typ)
}

// ringOrder scans the event ring: seqs strictly increase, and the frames
// named in want appear in that relative order (want entries match by type,
// except "tool_result:synth" which additionally requires the abort wording).
// It returns failure details, or "" when the order holds.
func ringOrder(r *Run, want ...string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lastSeq := int64(0)
	pos := 0
	var seqs []int64
	for _, ev := range r.history {
		if ev.Seq <= lastSeq {
			return "seq not monotonic"
		}
		lastSeq = ev.Seq
		seqs = append(seqs, ev.Seq)
		if pos >= len(want) {
			continue
		}
		w := want[pos]
		if ev.Type != strings.SplitN(w, ":", 2)[0] {
			continue
		}
		if strings.HasSuffix(w, ":synth") {
			data, _ := ev.Data.(toolResultPayload)
			if !strings.Contains(data.Content, "aborted") {
				continue
			}
		}
		pos++
	}
	if pos < len(want) {
		return "missing " + want[pos] + " in ring"
	}
	return ""
}

// Case: user cancel in the parked window emits the synthesized tool_result
// before the done frame (live tabs, reattach replay and the ledger mirror
// all see the open card close) and persists the four-segment derived
// history.
func TestCancelledRunWithDanglingToolCallSynthesizesResult(t *testing.T) {
	s, ts := newWakeServer(t, newTestAgent(t), nil) // parks on AskUserQuestion under ModeDefault
	saved := trackSaves(s)

	runID, sid, _ := postRun(t, ts.URL, "s1", "answer me this")
	awaitRunEvent(t, s, runID, EventInterrupt) // the ask parks: tool call committed, tools never dispatched
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	// Event order: the synthesized result sits between the real tool_call and
	// the terminal done; seq stays monotonic across the ring.
	if r := s.lookupRun(runID); r != nil {
		if msg := ringOrder(r, EventToolCall, "tool_result:synth", EventDone); msg != "" {
			r.mu.Lock()
			for _, ev := range r.history {
				t.Logf("  seq=%d type=%s data=%v", ev.Seq, ev.Type, ev.Data)
			}
			r.mu.Unlock()
			t.Fatalf("event ring: %s", msg)
		}
	} else {
		t.Fatal("run pruned before inspection")
	}

	// Persisted transcript: prompt + the committed AI tool-call message + the
	// synthesized Tool result + the stop notice.
	waitForTranscript(t, ts.URL, sid, "cancelled round partial + synth", func(msgs []messages.Message) bool {
		return len(msgs) == 4 &&
			msgs[0].Role == messages.RoleHuman && msgs[0].Content == "answer me this" &&
			msgs[1].Role == messages.RoleAI && len(msgs[1].ToolCalls) == 1
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if got := msgs[2]; got.Role != messages.RoleTool || got.ToolCallID != msgs[1].ToolCalls[0].ID ||
		!strings.HasPrefix(got.Content, "Tool call aborted by user after ") ||
		!strings.HasSuffix(got.Content, "— outcome unknown. Verify the effect before retrying.") {
		t.Fatalf("synth result = %+v, want the user-abort wording with elapsed", got)
	}
	if got := lastStopNotice(msgs); got != wantStopNotice(runID, StopReasonUser) {
		t.Fatalf("notice = %q, want the user_stop tag", got)
	}
	// The synth content must match the emitted event's content byte for byte.
	if r := s.lookupRun(runID); r != nil {
		r.mu.Lock()
		var emitted string
		for _, ev := range r.history {
			if data, ok := ev.Data.(toolResultPayload); ok && ev.Type == EventToolResult {
				emitted = data.Content
			}
		}
		r.mu.Unlock()
		if emitted != msgs[2].Content {
			t.Fatalf("emitted synth %q != persisted synth %q", emitted, msgs[2].Content)
		}
	}
}

// blockingTextModel streams one text chunk, then holds the stream open until
// the run ctx cancels — the cancel lands mid-text, before the model node
// commits its message, so the streamed text only exists in the delta buffer.
type blockingTextModel struct{}

func (m *blockingTextModel) Invoke(_ context.Context, _ []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return messages.AI("unused"), nil
}

func (m *blockingTextModel) Stream(_ context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &blockingTextStream{served: false}, nil
}

type blockingTextStream struct{ served bool }

func (s *blockingTextStream) Next(ctx context.Context) (messages.Message, bool, error) {
	if !s.served {
		s.served = true
		return messages.AI("partial answer"), true, nil
	}
	<-ctx.Done()
	return messages.Message{}, false, ctx.Err()
}

func (s *blockingTextStream) Close() error { return nil }

func (m *blockingTextModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}
func (m *blockingTextModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *blockingTextModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *blockingTextModel) BindTools(bound []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *blockingTextModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// Case: user cancel mid-stream persists the buffered partial text as a
// trailing AI message; no tool call existed, so no synthesized events.
func TestCancelledRunMidStreamPersistsPartialText(t *testing.T) {
	a, err := agent.New(agent.Config{
		Model:          &blockingTextModel{},
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, ts := newWakeServer(t, a, nil)
	saved := trackSaves(s)

	runID, sid, _ := postRun(t, ts.URL, "s2", "write me something")
	awaitRunEvent(t, s, runID, EventDelta)
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	waitForTranscript(t, ts.URL, sid, "partial streamed text", func(msgs []messages.Message) bool {
		return len(msgs) == 3 &&
			msgs[0].Role == messages.RoleHuman && msgs[0].Content == "write me something" &&
			msgs[1].Role == messages.RoleAI && msgs[1].Content == "partial answer"
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if got := lastStopNotice(msgs); got != wantStopNotice(runID, StopReasonUser) {
		t.Fatalf("notice = %q, want the user_stop tag", got)
	}
	if r := s.lookupRun(runID); r != nil {
		if msg := ringOrder(r, EventDelta, EventDone); msg != "" {
			t.Fatalf("event ring: %s", msg)
		}
		r.mu.Lock()
		for _, ev := range r.history {
			if ev.Type == EventToolResult {
				r.mu.Unlock()
				t.Fatalf("no dangling call existed, but a tool_result was emitted: %+v", ev.Data)
			}
		}
		r.mu.Unlock()
	}
}

// newCancelPartialGoalServer serves a goal-capable agent whose execute cycle
// parks on an ask-user approval (ModeDefault): the cycle's tool call is
// committed as a provisional GoalDelta message and the inner graph parks
// before the tools superstep runs — the deterministic dangling window.
func newCancelPartialGoalServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.AI("criteria checklist"), // assess
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{ // execute
				{ID: "g1", Name: "AskUserQuestion", Args: map[string]any{
					"questions": []any{map[string]any{"question": "pick one"}},
				}},
			}},
			messages.AI("implemented"),                              // unreached
			messages.AI(`{"verdict":"complete","evidence":"done"}`), // unreached
		),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return newWakeServer(t, a, nil)
}

// Case: goal cancel in the parked window (human=nil formula): the
// provisional GoalDelta message is the partial, the synth follows it, the
// notice closes.
func TestGoalCancelledRunPersistsPartialAndSynth(t *testing.T) {
	s, ts := newCancelPartialGoalServer(t)
	saved := trackSaves(s)
	const sid = "s-goal"

	_, runID, output := postGoal(t, ts.URL, sid, "set make the feature work")
	if runID == "" {
		t.Fatalf("goal set output = %q, want a started run", output)
	}
	awaitRunEvent(t, s, runID, EventInterrupt) // the cycle ask parks: g1 committed, never executed
	if err := postJSON(t, ts.URL+"/api/runs/"+runID+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, runID)
	awaitSave(t, saved, 1)

	// No human turn: derived = base(empty) + partial + synth + notice.
	waitForTranscript(t, ts.URL, sid, "goal partial + synth + notice", func(msgs []messages.Message) bool {
		return len(msgs) == 3 &&
			msgs[0].Role == messages.RoleAI && len(msgs[0].ToolCalls) == 1 && msgs[0].ToolCalls[0].ID == "g1"
	})
	msgs := sessionMessages(t, ts.URL, sid)
	if got := msgs[1]; got.Role != messages.RoleTool || got.ToolCallID != "g1" ||
		!strings.HasPrefix(got.Content, "Tool call aborted by user after ") {
		t.Fatalf("goal synth result = %+v, want the user-abort wording", got)
	}
	if got := lastStopNotice(msgs); got != wantStopNotice(runID, StopReasonUser) {
		t.Fatalf("notice = %q, want the user_stop tag", got)
	}
}
