package server

// run_partial_test.go pins the Run's partial-transcript accumulator (spec
// 2026-11-19-cancel-partial-output §D1): streamed text buffers until the
// owning node's updates batch lands the complete message; goal-cycle
// messages seen via GoalDelta custom chunks are provisional copies that the
// batch supersedes (the C1 dual-channel dedup — the goal graph emits every
// cycle message twice, and the partial transcript must keep each exactly
// once).

import (
	"context"
	"testing"
	"time"

	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
)

func customChunk(v any) lggraph.StreamChunk {
	return lggraph.StreamChunk{Mode: lggraph.StreamCustom, Payload: v}
}

func updatesChunk(msgs ...messages.Message) lggraph.StreamChunk {
	return lggraph.StreamChunk{Mode: lggraph.StreamUpdates, Payload: map[string]any{
		"node": map[string]any{"messages": msgs},
	}}
}

func newPartialRun() *Run {
	ctx, cancel := context.WithCancel(context.Background())
	return newRun("r-partial", nil, ctx, cancel)
}

// sameMessage compares the fields the accumulator preserves (messages carry
// uncomparable slices, so equality is field-wise).
func sameMessage(a, b messages.Message) bool {
	return a.Role == b.Role && a.Content == b.Content && a.ToolCallID == b.ToolCallID &&
		len(a.ToolCalls) == len(b.ToolCalls)
}

func partialMatches(got []messages.Message, want ...messages.Message) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !sameMessage(got[i], want[i]) {
			return false
		}
	}
	return true
}

// Streamed text buffers into a trailing AI message until the node's batch
// lands the complete message (with tool calls) and flushes the buffer.
func TestRunAccumulatorTextAndBatchSupersede(t *testing.T) {
	r := newPartialRun()
	r.render(customChunk("Hel"))
	r.render(customChunk("lo"))
	partial := r.partialTranscript()
	if len(partial) != 1 || partial[0].Role != messages.RoleAI || partial[0].Content != "Hello" {
		t.Fatalf("mid-stream partial = %v, want one AI('Hello')", partial)
	}
	ai := messages.AI("Hello")
	ai.ToolCalls = []messages.ToolCall{{ID: "c1", Name: "Read"}}
	r.render(updatesChunk(ai))
	partial = r.partialTranscript()
	if len(partial) != 1 || partial[0].Content != "Hello" || len(partial[0].ToolCalls) != 1 {
		t.Fatalf("post-batch partial = %v, want the committed AI message once", partial)
	}
}

// C1 pin: the goal graph delivers every execute-cycle message on TWO
// channels — a GoalDelta custom chunk while the cycle runs, and the outer
// node's updates batch when it commits. The SSE stream legitimately shows
// both (tool_call events ×2), but the partial transcript keeps each message
// exactly once: the batch supersedes the provisional copies.
func TestRunAccumulatorGoalDualChannelDedup(t *testing.T) {
	r := newPartialRun()
	ai := messages.AI("")
	ai.ToolCalls = []messages.ToolCall{{ID: "g1", Name: "Read"}}
	toolRes := messages.Tool("g1", "file body")

	// Mid-cycle window: messages streamed via GoalDelta only.
	r.render(customChunk(agent.GoalDelta{Message: ai}))
	r.render(customChunk(agent.GoalDelta{Message: toolRes}))
	partial := r.partialTranscript()
	if !partialMatches(partial, ai, toolRes) {
		t.Fatalf("mid-cycle partial = %v, want the two provisional messages once each", partial)
	}

	// The cycle commits: the outer batch re-delivers the same messages.
	r.render(updatesChunk(ai, toolRes))
	partial = r.partialTranscript()
	if !partialMatches(partial, ai, toolRes) {
		t.Fatalf("post-commit partial = %v, want each message exactly once (batch supersedes provisional)", partial)
	}
	// The SSE face DID double-emit — the dedup lives in the accumulator,
	// not in the event stream (reattach replay sees both passes).
	toolCalls, toolResults := 0, 0
	for _, ev := range r.history {
		switch ev.Type {
		case EventToolCall:
			toolCalls++
		case EventToolResult:
			toolResults++
		}
	}
	if toolCalls != 2 || toolResults != 2 {
		t.Fatalf("SSE counts = %d tool_call / %d tool_result, want 2/2 (dual channel)", toolCalls, toolResults)
	}
}

// A second goal cycle streaming after the first committed keeps its own
// provisional copy — the supersede rule is per-batch, not global.
func TestRunAccumulatorNextCycleProvisional(t *testing.T) {
	r := newPartialRun()
	ai1 := messages.AI("cycle one")
	r.render(customChunk(agent.GoalDelta{Message: ai1}))
	r.render(updatesChunk(ai1)) // cycle one commits

	ai2 := messages.AI("cycle two, uncommitted")
	r.render(customChunk(agent.GoalDelta{Message: ai2}))
	partial := r.partialTranscript()
	if len(partial) != 2 || partial[0].Content != "cycle one" || partial[1].Content != "cycle two, uncommitted" {
		t.Fatalf("partial = %v, want committed cycle one + provisional cycle two", partial)
	}
}

// GoalFinalReply text buffers like any streamed text.
func TestRunAccumulatorGoalFinalReplyBuffers(t *testing.T) {
	r := newPartialRun()
	r.render(customChunk(agent.GoalFinalReply{Text: "the answer"}))
	partial := r.partialTranscript()
	if len(partial) != 1 || partial[0].Content != "the answer" {
		t.Fatalf("partial = %v, want AI('the answer')", partial)
	}
}

// Runs without a render face (command runs) accumulate nothing.
func TestRunPartialEmptyWithoutRender(t *testing.T) {
	r := newPartialRun()
	if got := r.partialTranscript(); got != nil {
		t.Fatalf("partial = %v, want nil (nothing rendered)", got)
	}
}

// startWith stamps the run's start (post-promotion) for the elapsed copy.
func TestRunStartedAtStampedByStartWith(t *testing.T) {
	r := newPartialRun()
	if r.runElapsed() != 0 {
		t.Fatal("elapsed before startWith must be 0")
	}
	done := make(chan []messages.Message, 1)
	r.startWith(func() ([]messages.Message, error) { return nil, nil }, func(m []messages.Message) { done <- m })
	<-done
	if r.runElapsed() < 0 || r.runElapsed() > 5*time.Second {
		t.Fatalf("runElapsed = %v, want a small non-negative duration", r.runElapsed())
	}
}
