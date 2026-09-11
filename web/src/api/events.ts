// SSE event contract — mirrors the Event* constants in
// internal/server/ssevents.go. ci/check-sse-contract.sh fails CI when the
// two drift apart, so keep the name list below in exact sync and quote only
// event names with single quotes in this file.
import type { Goal } from '../types'

export const SSE_EVENT_TYPES = [
  'delta',
  'reasoning',
  'goal_note',
  'tool_call',
  'tool_result',
  'interrupt',
  'interrupt_resolved',
  'timeout',
  'goal',
  'run_queued',
  'queue_position',
  'run_started',
  'resync',
  'auth_revoked',
  'done',
  'error',
  'session_state',
] as const

export type SSEEventType = (typeof SSE_EVENT_TYPES)[number]

/** delta / reasoning / goal_note — streamed text chunk. */
export interface TextPayload {
  text?: string
}

export interface ToolCallPayload {
  id: string
  name: string
  args: Record<string, unknown>
}

export interface ToolResultPayload {
  tool_call_id: string
  content: string
}

/** payload is the parked request value; its shape depends on kind. */
export interface InterruptPayload {
  kind: string
  payload: unknown
}

export interface InterruptResolvedPayload {
  kind?: string
}

/** Parked-interrupt TTL fired server-side; the terminal done event follows. */
export interface TimeoutPayload {
  kind?: string
  reason?: string
}

export interface GoalPayload {
  goal: Goal | null
  running: boolean
}

/** run_queued / queue_position — admission-gate queue position. */
export interface QueuePositionPayload {
  position?: number
}

/** Replay gap or ledger failure — refetch a full snapshot. */
export interface ResyncPayload {
  reason?: string
}

export interface AuthRevokedPayload {
  code?: string
}

export interface DonePayload {
  cancelled?: boolean
}

export interface ErrorPayload {
  message?: string
}

/** Global stream (GET /api/events): one session's run state. */
export interface SessionStatePayload {
  session_id: string
  status: string
  run_id?: string
}

export interface SSEEventPayloadMap {
  delta: TextPayload
  reasoning: TextPayload
  goal_note: TextPayload
  tool_call: ToolCallPayload
  tool_result: ToolResultPayload
  interrupt: InterruptPayload
  interrupt_resolved: InterruptResolvedPayload
  timeout: TimeoutPayload
  goal: GoalPayload
  run_queued: QueuePositionPayload
  queue_position: QueuePositionPayload
  run_started: Record<string, never>
  resync: ResyncPayload
  auth_revoked: AuthRevokedPayload
  done: DonePayload
  error: ErrorPayload
  session_state: SessionStatePayload
}

export type SSEEventPayload<T extends SSEEventType> = SSEEventPayloadMap[T]
