package server

// SSE event names for the run-events stream (GET /api/runs/{id}/events) and
// the global session-state stream (GET /api/events). web/src/api/events.ts
// mirrors this list; ci/check-sse-contract.sh fails CI when the two drift.
// The fs-watch stream (handleFSWatch) is a separate endpoint outside this
// contract — its event name (dir-changed) predates and does not fit the
// snake_case vocabulary.
const (
	EventDelta             = "delta"
	EventReasoning         = "reasoning"
	EventGoalNote          = "goal_note"
	EventToolCall          = "tool_call"
	EventToolResult        = "tool_result"
	EventInterrupt         = "interrupt"
	EventInterruptResolved = "interrupt_resolved"
	EventTimeout           = "timeout"
	EventGoal              = "goal"
	EventRunQueued         = "run_queued"
	EventQueuePosition     = "queue_position"
	EventRunStarted        = "run_started"
	EventResync            = "resync"
	EventAuthRevoked       = "auth_revoked"
	EventDone              = "done"
	EventError             = "error"
	EventSessionState      = "session_state"
)

// toolCallPayload is the data of a tool_call event.
type toolCallPayload struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// toolResultPayload is the data of a tool_result event.
type toolResultPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// interruptPayload is the data of an interrupt event; Payload is the
// parked request value (permission/ask/plan), whose shape depends on Kind.
type interruptPayload struct {
	Kind    string `json:"kind"`
	Payload any    `json:"payload"`
}
