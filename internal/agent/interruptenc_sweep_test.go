package agent

import (
	"encoding/json"
	"testing"

	"github.com/projanvil/woca/internal/tools"
)

// encodeInterruptPayload / decodeInterruptPayload round-trip all payload
// shapes; derefAny covers pointer/nil/plain branches.
func TestInterruptEncodeDecodeSweep(t *testing.T) {
	pr := PermissionRequest{ToolName: "Bash", Reason: "test"}
	enc := encodeInterruptPayload(pr)
	dec := decodeInterruptPayload(enc)
	if _, ok := dec.(PermissionRequest); !ok {
		t.Fatalf("decoded = %T", dec)
	}

	ar := AskUserRequest{Questions: []tools.Question{{Question: "hi"}}}
	enc2 := encodeInterruptPayload(ar)
	dec2 := decodeInterruptPayload(enc2)
	if _, ok := dec2.(AskUserRequest); !ok {
		t.Fatalf("decoded = %T", dec2)
	}

	pa := PlanApprovalRequest{Plan: "do it"}
	enc3 := encodeInterruptPayload(pa)
	dec3 := decodeInterruptPayload(enc3)
	if _, ok := dec3.(PlanApprovalRequest); !ok {
		t.Fatalf("decoded = %T", dec3)
	}

	appr := ApprovalRequest{Asks: []Ask{{
		CallID: "1", ToolName: "Bash", ToolArgs: map[string]any{"command": "make"},
		Origin: OriginMain, SessionRuleKey: "bash:make*",
	}}}
	enc4 := encodeInterruptPayload(appr)
	dec4 := decodeInterruptPayload(enc4)
	if _, ok := dec4.(ApprovalRequest); !ok {
		t.Fatalf("decoded = %T", dec4)
	}

	ans := ApprovalAnswer{Decisions: map[string]AskDecision{
		"1": {Outcome: OutcomeAllow, Grant: GrantSession},
	}}
	encAns := encodeResumeValue(ans)
	if _, ok := decodeResumeValue(encAns).(ApprovalAnswer); !ok {
		t.Fatalf("ApprovalAnswer resume round-trip failed: %T", decodeResumeValue(encAns))
	}

	// derefAny branches.
	if v := derefAny("plain"); v != "plain" {
		t.Fatalf("derefAny(str) = %v", v)
	}
	if v := derefAny(&pr); v == nil {
		t.Fatal("derefAny(ptr) = nil")
	}
	if v := derefAny(nil); v != nil {
		t.Fatal("derefAny(nil) != nil")
	}

	// encodeResumeValue / decodeResumeValue round-trips.
	for _, v := range []any{"y", "n", AskUserAnswer{Answers: []string{"a"}}, ApprovalAnswer{Decisions: map[string]AskDecision{"1": {Outcome: OutcomeDeny}}}, nil} {
		enc := encodeResumeValue(v)
		dec := decodeResumeValue(enc)
		_ = dec
	}

	// Round-trip through JSON (the wire format).
	wire, err := json.Marshal(encodeResumeValue("y"))
	if err != nil {
		t.Fatal(err)
	}
	var back string
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	decoded := decodeResumeValue(back)
	_ = decoded
}
