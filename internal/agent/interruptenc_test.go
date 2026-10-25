package agent

import (
	"encoding/json"
	"testing"
)

func TestSubagentInterruptRoundTrip(t *testing.T) {
	orig := SubagentInterrupt{
		Subagent:   "researcher",
		ToolCallID: "call_123",
		ThreadID:   "run-abc/sub-call_123",
		Inner: PermissionRequest{
			ToolName: "Write", Reason: "write file",
			ToolArgs: map[string]any{"path": "/tmp/x"},
		},
	}
	enc := encodeInterruptPayload(orig)
	// Simulate the JSON round-trip through a persistent saver.
	blob, err := json.Marshal(enc)
	if err != nil {
		t.Fatal(err)
	}
	var back any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	got, ok := decodeInterruptPayload(back).(SubagentInterrupt)
	if !ok {
		t.Fatalf("decoded %T, want SubagentInterrupt", decodeInterruptPayload(back))
	}
	if got.Subagent != orig.Subagent || got.ToolCallID != orig.ToolCallID ||
		got.ThreadID != orig.ThreadID || got.Inner.ToolName != "Write" ||
		got.Inner.ToolArgs["path"] != "/tmp/x" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
