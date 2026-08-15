// Shared domain types for the woca web UI. These mirror the Go server's JSON.

export interface Session {
  id: string
  title: string
  updatedAt: string
  /** Whether the transcript has been fetched from the backend. */
  loaded: boolean
  /** Absolute workdir path this session belongs to ('' when unknown). */
  workspace: string
  /** Display name of the workspace (project dir basename). */
  workspaceName: string
}

/** A named group of sessions sharing one workspace. */
export interface WorkspaceGroup {
  /** Unique key: workspace path (or display name as fallback). */
  key: string
  /** Workspace absolute path ('' for the unnamed default group). */
  path: string
  name: string
  sessions: Session[]
}

// A conversation is a flat, ordered list of items produced by the agent.
export type ChatItem =
  | { id: string; kind: 'user'; content: string; attachments?: Attachment[] }
  | {
      id: string
      kind: 'assistant'
      content: string
      streaming: boolean
      /** Unix ms when the bubble first appeared (debug copy button). */
      ts?: number
      /** Backend run that produced this bubble (debug copy button). */
      srcRun?: string
    }
  | { id: string; kind: 'tool_call'; toolCallId: string; name: string; args: Record<string, unknown> }
  | { id: string; kind: 'tool_result'; toolCallId: string; content: string }

/** An image attached to a prompt (paste or file picker). */
export interface Attachment {
  id: string
  name: string
  mimeType: string
  /** `data:image/png;base64,…` — used directly in <img :src>. */
  dataUrl: string
  /** Bare base64 payload sent to the run API. */
  base64: string
}

/** One autocomplete entry (skill or subagent) with its description. */
export interface CompletionEntry {
  name: string
  description: string
}

export type RunStatus = 'idle' | 'running' | 'awaiting' | 'done' | 'error'

// --- goal mode (mirror the Go agent's Goal JSON) ---

export type GoalStatus = 'active' | 'paused' | 'blocked' | 'complete' | 'budget_limited'

export interface GoalProgressEntry {
  cycle: number
  summary: string
  verdict: string
  note?: string
}

export interface Goal {
  objective: string
  status: GoalStatus
  session_id?: string
  cycles: number
  max_cycles: number
  tokens_used: number
  max_tokens: number
  progress?: GoalProgressEntry[]
  notes?: string[]
  /** Executor's completion claim awaiting skeptical verification. */
  claimed?: boolean
  claim_evidence?: string
  blocked_reason?: string
  evidence?: string
  created_at: string
  updated_at: string
}

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
  /** Multimodal content blocks ({type:"image", mime_type, base64} etc.). */
  content_blocks?: Array<{
    type?: string
    mime_type?: string
    base64?: string
    text?: string
  }>
}

export interface SessionSummary {
  id: string
  title: string
  updated_at: string
  message_count: number
  workspace?: string
  workspace_name?: string
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
  /** Harness observability log (graph/state/session/message traces). */
  debug_log: boolean
  /** Where observability logs live (~/.woca/logs), empty when unwired. */
  log_dir: string
  memory_engine: string
  ladym_db_path: string
  ladym_embedding: string
  ladym_embedding_model: string
  ladym_embedding_base_url: string
  ladym_embedding_api_key_set: boolean
  ladym_llm: string
  ladym_autoretain: boolean
  ladym_topk: number
  ladym_system2: boolean
  ladym_system2_interval: number
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
  debug_log?: boolean
  memory_engine?: string
  ladym_db_path?: string
  ladym_embedding?: string
  ladym_embedding_model?: string
  ladym_embedding_base_url?: string
  ladym_embedding_api_key?: string
  ladym_llm?: string
  ladym_autoretain?: boolean
  ladym_topk?: number
  ladym_system2?: boolean
  ladym_system2_interval?: number
}

// --- memory (mirror /api/memory) ---

export interface MemoryItem {
  id: string
  layer: string
  type: string
  summary: string
  content: string
  source: string
  tags: string[] | null
  score?: number
  updated_at: number
}

export interface MemoryStats {
  total: number
  layers: Record<string, number>
}

export interface MemoryListResponse {
  engine: string
  items: MemoryItem[]
  total: number
}

export interface MemoryStatsResponse {
  engine: string
  stats: MemoryStats | null
  workspace?: string
}

export interface RememberResponse {
  id: string
  dropped?: boolean
}
