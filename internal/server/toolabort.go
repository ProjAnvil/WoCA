package server

// toolabort.go synthesizes terminal tool results for tool calls left
// dangling by a run stop (spec 2026-11-19-cancel-partial-output §D2). A
// cancelled run persists its partial transcript; every tool call without an
// accumulated result gets a Tool message whose content names the abort
// reason, the elapsed time and the verify-before-retry guidance — the data
// layer closes the call, so no consumer ever meets a dangling tool_use
// (the shape providers reject, and the shape the web renders as a spinner).
//
// The wording deliberately diverges from the existing "Error:"-prefixed
// synthesized results (answerDanglingToolCalls, the ε-gate crash marker):
// an abort is not an error, and the content must never read as a
// machine-notice tag.

import (
	"fmt"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
)

// synthToolAbortContent renders the synthesized tool-result body for one
// aborted call. reason is one of the StopReason* closed set (unknown values
// degrade to the user phrasing, matching cancelledReason's fallback);
// elapsed is measured from run start (post-promotion — queue wait excluded,
// parked waits included) and rendered with one decimal.
func synthToolAbortContent(reason string, elapsed time.Duration) string {
	secs := fmt.Sprintf("%.1fs", elapsed.Seconds())
	switch reason {
	case StopReasonParkedTTL:
		return "Tool call aborted: approval timed out — outcome unknown. Verify the effect before retrying."
	case StopReasonServerStop:
		return "Tool call aborted by server shutdown — outcome unknown. Verify the effect before retrying."
	case StopReasonLeaseLost:
		return "Tool call aborted: run lease lost to another instance — outcome unknown. Verify the effect before retrying."
	default:
		return fmt.Sprintf("Tool call aborted by user after %s — outcome unknown. Verify the effect before retrying.", secs)
	}
}

// danglingToolCalls reports the tool calls of the partial transcript that
// never received a Tool result, in stream order (call ids are unique per
// model emission, so an id answered anywhere is answered).
func danglingToolCalls(partial []messages.Message) []messages.ToolCall {
	answered := map[string]bool{}
	for _, m := range partial {
		if m.Role == messages.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var dangling []messages.ToolCall
	seen := map[string]bool{}
	for _, m := range partial {
		if m.Role != messages.RoleAI {
			continue
		}
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] || seen[tc.ID] {
				continue
			}
			seen[tc.ID] = true
			dangling = append(dangling, tc)
		}
	}
	return dangling
}

// synthesizeAborted builds the synthesized Tool messages for every dangling
// call of the partial transcript, in call order.
func synthesizeAborted(partial []messages.Message, reason string, elapsed time.Duration) []messages.Message {
	dangling := danglingToolCalls(partial)
	if len(dangling) == 0 {
		return nil
	}
	synth := make([]messages.Message, 0, len(dangling))
	for _, tc := range dangling {
		synth = append(synth, messages.Tool(tc.ID, synthToolAbortContent(reason, elapsed)))
	}
	return synth
}
