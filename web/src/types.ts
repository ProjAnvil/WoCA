// Shared domain types for the woca web UI. These mirror the Go server's JSON.

export interface Session {
  id: string
  title: string
  updatedAt: string
  /** Whether the transcript has been fetched from the backend. */
  loaded: boolean
}

// A conversation is a flat, ordered list of items produced by the agent.
export type ChatItem =
  | { id: string; kind: 'user'; content: string }
  | { id: string; kind: 'assistant'; content: string; streaming: boolean }
  | { id: string; kind: 'tool_call'; toolCallId: string; name: string; args: Record<string, unknown> }
  | { id: string; kind: 'tool_result'; toolCallId: string; content: string }

export type RunStatus = 'idle' | 'running' | 'awaiting' | 'done' | 'error'

// --- interrupts (mirror the Go server's JSON payloads) ---

export interface QuestionOption {
  label: string
  description?: string
}

export interface Question {
  question: string
  header?: string
  options?: QuestionOption[]
  multi_select?: boolean
}

export interface PermissionPayload {
  tool_name: string
  tool_args: Record<string, unknown>
  reason: string
  is_read_only: boolean
}

export interface AskPayload {
  questions: Question[]
}

export interface PlanPayload {
  plan: string
}

export type Interrupt =
  | { kind: 'permission'; payload: PermissionPayload }
  | { kind: 'ask'; payload: AskPayload }
  | { kind: 'plan'; payload: PlanPayload }

// --- transcript (the Go server's messages.Message JSON shape) ---

export interface TranscriptMessage {
  role: string
  content?: string
  tool_calls?: Array<{ id?: string; name: string; args?: Record<string, unknown> }>
  tool_call_id?: string
}

export interface SessionSummary {
  id: string
  title: string
  updated_at: string
  message_count: number
}

// --- workspace (markdown preview) ---

export type FileKind = 'dir' | 'file'

export interface WorkspaceFile {
  name: string
  path: string
  kind: FileKind
  children?: WorkspaceFile[]
  content?: string
}

// --- settings (mirror the Go server's /api/settings JSON) ---

export type CheckpointBackend = 'none' | 'file' | 'sqlite' | 'postgres'
export type MemoryBackend = 'none' | 'memory' | 'sqlite' | 'postgres'

export interface AppSettings {
  provider: 'anthropic' | 'openai'
  model: string
  max_tokens: number
  temperature: number
  anthropic_base_url: string
  openai_base_url: string
  anthropic_api_key_set: boolean
  openai_api_key_set: boolean
  checkpoint_backend: CheckpointBackend | string
  memory_backend: MemoryBackend | string
  checkpoint_dir: string
  postgres_url_set: boolean
  database: string
}

/** PUT /api/settings body — absent fields are unchanged; empty strings clear. */
export interface AppSettingsUpdate {
  provider?: string
  model?: string
  max_tokens?: number
  temperature?: number
  anthropic_base_url?: string
  openai_base_url?: string
  anthropic_api_key?: string
  openai_api_key?: string
  checkpoint_backend?: string
  memory_backend?: string
  checkpoint_dir?: string
  postgres_url?: string
}
