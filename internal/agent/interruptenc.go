package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Interrupt payload envelope: the fork's checkpoint serde covers langchain
// core types only, and its types.Interrupt envelope encodes the nested Value
// through that same closed registry — so a raw woca struct as Interrupt's
// value breaks durable savers. The langgraph payload contract is
// "JSON-serializable" anyway: encode payloads (and struct-shaped resume
// answers) into a JSON-native map tagged with the concrete type, and decode
// back at the boundaries. In-process flows are unaffected either way; the
// envelope is what makes cross-process resume lossless.

// wocaInterruptTag is the map key carrying the concrete payload type.
const wocaInterruptTag = "__woca_interrupt__"

// wocaResumePrefix introduces the string envelope for struct-shaped resume
// answers. A resume value must stay scalar-shaped: the fork reads a
// map[string]any resume as an interrupt-ID map, so struct answers cannot use
// the map envelope.
const wocaResumePrefix = "__woca_resume__:"

// encodeInterruptPayload converts a woca interrupt payload (or resume
// answer) into a JSON-native envelope map. Non-struct values (bool, string)
// and unknown shapes pass through unchanged.
func encodeInterruptPayload(v any) any {
	switch v.(type) {
	case PermissionRequest, AskUserRequest, AskUserAnswer, PlanApprovalRequest, ApprovalRequest, ApprovalAnswer, GoalDelta, SubagentInterrupt, MultiInterrupt:
	default:
		return v // JSON-native or already envelope-shaped
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return v // encoding failures degrade to pass-through; in-memory flows still work
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		return v
	}
	if m == nil {
		m = map[string]any{}
	}
	m[wocaInterruptTag] = fmt.Sprintf("%T", v)
	return m
}

// decodeInterruptPayload restores the concrete payload type from an envelope
// produced by encodeInterruptPayload. Non-envelope values pass through.
func decodeInterruptPayload(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	kind, _ := m[wocaInterruptTag].(string)
	if kind == "" {
		return v
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return v
	}
	var out any
	switch kind {
	case "agent.PermissionRequest":
		out = &PermissionRequest{}
	case "agent.AskUserRequest":
		out = &AskUserRequest{}
	case "agent.AskUserAnswer":
		out = &AskUserAnswer{}
	case "agent.PlanApprovalRequest":
		out = &PlanApprovalRequest{}
	case "agent.ApprovalRequest":
		out = &ApprovalRequest{}
	case "agent.ApprovalAnswer":
		out = &ApprovalAnswer{}
	case "agent.GoalDelta":
		out = &GoalDelta{}
	case "agent.SubagentInterrupt":
		out = &SubagentInterrupt{}
	case "agent.MultiInterrupt":
		out = &MultiInterrupt{}
	default:
		return v
	}
	if err := json.Unmarshal(blob, out); err != nil {
		return v
	}
	if agg, ok := out.(*MultiInterrupt); ok {
		// The aggregate's inner values ride as raw envelopes (the goal relay
		// copies them verbatim from the inner pause): decode each card so the
		// surfaced MultiInterrupt is fully decoded, like every other payload.
		for i := range agg.Interrupts {
			agg.Interrupts[i].Value = decodeInterruptPayload(agg.Interrupts[i].Value)
		}
	}
	return derefAny(out)
}

// derefAny returns the pointed-to value of a pointer (or v unchanged).
func derefAny(v any) any {
	switch p := v.(type) {
	case *PermissionRequest:
		return *p
	case *AskUserRequest:
		return *p
	case *AskUserAnswer:
		return *p
	case *PlanApprovalRequest:
		return *p
	case *ApprovalRequest:
		return *p
	case *ApprovalAnswer:
		return *p
	case *GoalDelta:
		return *p
	case *SubagentInterrupt:
		return *p
	case *MultiInterrupt:
		return *p
	}
	return v
}

// encodeResumeValue wraps struct-shaped resume answers in a scalar string
// envelope so they survive durable persistence without colliding with the
// fork's interrupt-ID-map resume semantics. Other values pass through. An
// NS-keyed map answer (multi-interrupt resume, §3.4) keeps its shape — the
// fork needs the map — but each VALUE is enveloped individually so the
// consumed-prefix writes persist losslessly across processes.
func encodeResumeValue(v any) any {
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = encodeResumeValue(val)
		}
		return out
	}
	switch v.(type) {
	case AskUserAnswer, ApprovalAnswer:
	default:
		return v
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return v
	}
	return wocaResumePrefix + fmt.Sprintf("%T", v) + ":" + string(blob)
}

// decodeResumeValue restores a resume answer from encodeResumeValue's
// envelope; other values pass through. Map answers decode each value.
func decodeResumeValue(v any) any {
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = decodeResumeValue(val)
		}
		return out
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	rest, ok := strings.CutPrefix(s, wocaResumePrefix)
	if !ok {
		return v
	}
	kind, blob, ok := strings.Cut(rest, ":")
	if !ok {
		return v
	}
	var out any
	switch kind {
	case "agent.AskUserAnswer":
		out = &AskUserAnswer{}
	case "agent.ApprovalAnswer":
		out = &ApprovalAnswer{}
	default:
		return v
	}
	if err := json.Unmarshal([]byte(blob), out); err != nil {
		return v
	}
	return derefAny(out)
}

// DecodeInterruptValue restores a concrete interrupt payload from its
// durable-persistence envelope (see encodeInterruptPayload). The harness's
// interrupt handlers (runner, server park) receive decoded values.
func DecodeInterruptValue(v any) any { return decodeInterruptPayload(v) }
