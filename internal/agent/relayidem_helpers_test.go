package agent

// Shared helpers for the revived relay-idempotency repro tests (N5 in
// relayidem_n5_test.go, X1 in sessionpolicy_test.go): transcript logging the
// repros use to document what the harness actually did.

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

// logToolMsgs logs every tool result of a final transcript (truncated),
// labeling the run: the repro tests' evidence trail for which calls executed
// and which were denied.
func logToolMsgs(t *testing.T, label string, msgs []messages.Message) {
	t.Helper()
	for _, m := range msgs {
		if m.Role != messages.RoleTool {
			continue
		}
		c := m.Content
		if len(c) > 160 {
			c = c[:160] + "…"
		}
		t.Logf("%s tool[%s]: %q", label, m.ToolCallID, c)
	}
}
