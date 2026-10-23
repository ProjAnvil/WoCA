package agent

import (
	"context"
	"slices"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Max-output-tokens recovery for a truncated response (the withheld
// max_output_tokens error): when the final assistant message was cut off by
// the output-token cap, the turn is retried
// — first once at an escalated cap when an escalator is configured (the 8k
// →64k retry; escalate only when the user did not set an explicit cap),
// then up to MaxOutputTokensRecoveries times by injecting a
// continuation message that asks the model to resume mid-thought. Recovery
// exhausted, the truncated output is surfaced as-is.

const (
	// MaxOutputTokensRecoveries bounds the continuation-message retries.
	MaxOutputTokensRecoveries = 3
	// EscalatedMaxTokens is the raised output cap for the escalation retry.
	EscalatedMaxTokens = 64_000
)

// OutputLimitRecoveryPrompt is the continuation prompt injected on recovery.
const OutputLimitRecoveryPrompt = "Output token limit hit. Resume directly — no apology, no recap of what you were doing. Pick up mid-thought if that is where the cut happened. Break remaining work into smaller pieces."

// IsMaxOutputTokensTruncated reports whether an AI message was cut off by
// the max-output-tokens cap. Anthropic surfaces this as stop_reason
// "max_tokens"; the OpenAI convention is finish_reason "length".
func IsMaxOutputTokensTruncated(m messages.Message) bool {
	if m.Role != messages.RoleAI {
		return false
	}
	if sr, ok := m.ResponseMetadata["stop_reason"].(string); ok && sr == "max_tokens" {
		return true
	}
	if fr, ok := m.ResponseMetadata["finish_reason"].(string); ok && fr == "length" {
		return true
	}
	return false
}

// TruncatedTail reports whether the last AI message of a transcript was
// truncated by the output-token cap.
func TruncatedTail(msgs []messages.Message) bool {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role == messages.RoleAI {
			return IsMaxOutputTokensTruncated(msg)
		}
		if msg.Role == messages.RoleHuman || msg.Role == messages.RoleTool {
			return false
		}
	}
	return false
}

// RecoverOutputLimit drives the retry loop over a completed turn: history is
// the turn's original input (without system prompt), out its transcript, and
// turn re-runs the graph. The escalation swap (when configured and not yet
// used this turn) is restored when the loop exits.
func (a *Agent) RecoverOutputLimit(ctx context.Context, history []messages.Message, out []messages.Message, turn func(context.Context, []messages.Message) ([]messages.Message, error)) ([]messages.Message, error) {
	var restore func()
	defer func() {
		if restore != nil {
			restore()
		}
	}()

	escalated := false
	recoveries := 0
	for TruncatedTail(out) {
		if !escalated {
			if restoreFn, ok := a.BeginOutputEscalation(); ok {
				escalated = true
				restore = restoreFn
				next, err := turn(ctx, history)
				if err != nil {
					return out, err
				}
				out = next
				continue
			}
		}
		if recoveries >= MaxOutputTokensRecoveries {
			break
		}
		recoveries++
		next, err := turn(ctx, append(stripSystemMessages(out), messages.Human(OutputLimitRecoveryPrompt)))
		if err != nil {
			return out, err
		}
		out = next
	}
	return out, nil
}

// BeginOutputEscalation swaps in a model built at the escalated output cap
// and returns the restore function to call when the turn ends. It reports
// ok=false when no escalator is configured or the swap failed. Also used by
// the runner at the turn boundary of the streaming path.
func (a *Agent) BeginOutputEscalation() (restore func(), ok bool) {
	if a.escalate == nil {
		return nil, false
	}
	m, err := a.escalate()
	if err != nil || m == nil {
		return nil, false
	}
	prev := a.CurrentModel()
	if err := a.SetModel(m); err != nil {
		return nil, false
	}
	return func() { _ = a.SetModel(prev) }, true
}

// stripSystemMessages removes leading system messages so a completed
// transcript can be replayed as turn history.
func stripSystemMessages(msgs []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != messages.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}
