package agent

// The ε-min resume gate (design §3.5, Task 9). The entry points Resume,
// ResumeStream and ResumeCheckpoint inspect the thread's pause snapshot —
// through the compiled graph's GetState, i.e. the fork checkpoint shape
// (Checkpoint.Next + PendingWrites) — and divert two thread shapes away from
// the graph's native resume, because native resume would RE-EXECUTE work
// whose outcome is unknown:
//
//   - CRASH face: an unfinished EXECUTION superstep (Checkpoint.Next plans
//     the tools node or a subagent fan-out task) with missing tool results
//     and NO pending interrupt. Tool results persist only at superstep
//     commit, so this is exactly the shape a process death (or a hard
//     abort) mid-tools leaves behind: the calls may or may not have run,
//     and their results are gone. Resuming natively would re-dispatch the
//     tools task and execute the batch again (the R2 defect).
//   - LEGACY face: a pending interrupt whose value decodes to the retired
//     relay-era SubagentInterrupt envelope. In the new topology interrupts
//     never live on an execution node, so such a checkpoint predates the
//     Send fan-out; a native resume re-runs the tools task, re-executing
//     already-run siblings (their results rode the relay's in-memory memo)
//     and falling into runTask's routing tombstone for the Task call (the
//     R3 defect, parked from T5/T7).
//
// Both faces close the turn interrupted instead: one marker tool result per
// unanswered call of the paused batch, written into the thread as the tools
// node's state update (CompiledGraph.UpdateState), then the run continues
// from the updated checkpoint with a nil resume — the model gets exactly
// ONE round that sees the markers and ends the turn (or asks fresh work,
// which goes through the full gates; the paused batch itself never runs).
// The Task 5 tools-node legacy guard stays as the in-node backstop; this is
// the entry-point gate.

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
)

// crashInterruptedMarker is the tool result synthesized for calls whose
// execution superstep never completed (the crash face): the process died
// between the approval commit and the tools commit, so the call may or may
// not have run — never re-execute it blindly.
const crashInterruptedMarker = "Error: interrupted before completion; the tool may or may not have run. Verify before retrying."

// closeInterruptedTurn runs the ε-min entry gate on threadID: when the
// thread's latest checkpoint shows the crash or legacy face, it synthesizes
// interrupted marker results for every unanswered call of the paused batch
// and records them as a tools-node state update, so the caller's subsequent
// nil-input continuation runs ONE model round over the markers and finishes.
// It reports whether the turn was closed this way. Legal pauses (any pending
// interrupt that is not the retired envelope), finished threads, threads
// paused before the execution superstep (nothing could have executed yet —
// a plain resume is safe), and unreadable/absent snapshots all pass through
// unchanged; the graph surfaces its own resume semantics for them.
func (a *Agent) closeInterruptedTurn(ctx context.Context, g *lggraph.CompiledGraph, threadID string) (bool, error) {
	if a.saver == nil {
		return false, nil
	}
	snap, err := g.GetState(ctx, lgcheckpoint.Config{ThreadID: threadID})
	if err != nil {
		// No checkpoint for the thread (or the snapshot cannot be read):
		// fall through and let the graph's own resume error surface.
		return false, nil
	}
	calls := unansweredBatchCalls(snap.Values)
	if len(calls) == 0 {
		return false, nil // no paused batch to close out
	}
	legacy := false
	for _, intr := range snap.Interrupts {
		if _, ok := decodeInterruptPayload(intr.Value).(SubagentInterrupt); ok {
			legacy = true
			break
		}
	}
	if !legacy {
		if len(snap.Interrupts) > 0 {
			return false, nil // a legal pause: resume it natively
		}
		if !executionSuperstepPending(snap.Next) {
			// The unfinished superstep is a gate/model round (or the thread
			// already finished): nothing below the gates could have executed,
			// so a native resume re-runs nothing with an unknown outcome.
			return false, nil
		}
	}
	marker := crashInterruptedMarker
	if legacy {
		marker = legacyInterruptedMarker
	}
	if _, err := g.UpdateState(ctx, snap.Config, map[string]any{"messages": markerResults(calls, marker)}, nodeTools); err != nil {
		return false, fmt.Errorf("agent: closing interrupted turn for thread %q: %w", threadID, err)
	}
	if a.obs != nil {
		face := "crash"
		if legacy {
			face = "legacy"
		}
		a.obs.Log("warn", "interrupted_marker_resume", map[string]any{
			"thread": threadID,
			"face":   face,
			"calls":  len(calls),
		})
	}
	return true, nil
}

// unansweredBatchCalls returns the paused batch's unanswered tool calls: the
// calls of the LAST AI message carrying tool calls that have no tool result
// anywhere in the thread state. Call IDs are unique per conversation, so a
// result message anywhere answers its call.
func unansweredBatchCalls(state map[string]any) []messages.ToolCall {
	msgs := MessagesFrom(state)
	answered := make(map[string]bool, len(msgs))
	var batch *messages.Message
	for i := range msgs {
		switch msgs[i].Role {
		case messages.RoleTool:
			answered[msgs[i].ToolCallID] = true
		case messages.RoleAI:
			if len(msgs[i].ToolCalls) > 0 {
				batch = &msgs[i]
			}
		}
	}
	if batch == nil {
		return nil
	}
	var out []messages.ToolCall
	for _, call := range batch.ToolCalls {
		if !answered[call.ID] {
			out = append(out, call)
		}
	}
	return out
}

// executionSuperstepPending reports whether the checkpoint's planned
// superstep contains the tools node or a subagent fan-out task — the
// superstep whose calls may have partially executed before the process died.
func executionSuperstepPending(next []string) bool {
	for _, n := range next {
		if n == nodeTools || strings.HasPrefix(n, subagentNodePrefix) {
			return true
		}
	}
	return false
}

// markerResults synthesizes one interrupted tool result per call.
func markerResults(calls []messages.ToolCall, text string) []messages.Message {
	out := make([]messages.Message, len(calls))
	for i, call := range calls {
		out[i] = messages.Tool(call.ID, text)
	}
	return out
}

// finishResumedTurn continues a gate-diverted thread (Resume's invoke
// counterpart): the markers are already recorded as the tools node's state
// update, so a nil-input invoke runs the one model round that sees them and
// returns the turn's transcript. A pause surfacing in that closing round
// (the model asking fresh work) parks with the usual GraphInterrupt shape.
func (a *Agent) finishResumedTurn(ctx context.Context, g *lggraph.CompiledGraph, threadID string) ([]messages.Message, error) {
	res, err := g.InvokeWithOptions(ctx, nil, lggraph.Options{ThreadID: threadID})
	if err != nil {
		return nil, err
	}
	if len(res.Interrupts) > 0 {
		return nil, &lgtypes.GraphInterrupt{Interrupt: res.Interrupts[0]}
	}
	return MessagesFrom(res.Values), nil
}
