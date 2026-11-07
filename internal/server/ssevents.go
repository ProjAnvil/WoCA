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
	EventWakeCapped        = "wake_capped"
	EventActivityChanged   = "activity_changed"
)

// toolCallPayload is the data of a tool_call event. Fields are declared in
// JSON-key alphabetical order so encoding/json emits the same key order the
// previous map[string]any payload did (maps marshal with sorted keys).
type toolCallPayload struct {
	Args map[string]any `json:"args"`
	ID   string         `json:"id"`
	Name string         `json:"name"`
}

// toolResultPayload is the data of a tool_result event. Fields are declared
// in JSON-key alphabetical order for the same byte-identity reason.
type toolResultPayload struct {
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

// interruptPayload is the data of an interrupt event; Payload is the
// parked request value (permission/ask/plan), whose shape depends on Kind.
type interruptPayload struct {
	Kind    string `json:"kind"`
	Payload any    `json:"payload"`
}

// activityChangedPayload is the data of an activity_changed event on the
// global stream (PM §4.4): which session's activity changed and which
// slice. It carries no content — clients pull the snapshot through the
// authorized REST route, so the broadcast never leaks commands, todo text
// or cron prompts to clients that cannot read the session (D4). kind is
// one of the agent.ActivityKind values (todos|shells|tasks|crons) plus
// "wake", which the server's wake driver emits itself.
type activityChangedPayload struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
}
