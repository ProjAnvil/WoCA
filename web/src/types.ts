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
  | {
      id: string
      kind: 'reasoning'
      content: string
      /** True while the model is still streaming this thinking segment. */
      streaming: boolean
    }
  | {
      id: string
      kind: 'goal_note'
      content: string
      /** True while the goal loop is still appending to this block. */
      streaming: boolean
    }
  | {
      id: string
      kind: 'tool_call'
      toolCallId: string
      name: string
      args: Record<string, unknown>
      /** Tool output merged onto the call's card once it arrives. */
      result?: string
    }
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

export type RunStatus = 'idle' | 'queued' | 'running' | 'awaiting' | 'done' | 'error'

/** POST /api/runs response — state is 'queued' when another run holds the session gate. */
export interface CreateRunResponse {
  run_id: string
  session_id: string
  state: 'queued' | 'running'
  queue_position: number
}

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
    /** ReasoningBlock text (thinking / reasoning_content). */
    reasoning?: string
  }>
}

export interface SessionSummary {
  id: string
  title: string
  updated_at: string
  message_count: number
  workspace?: string
  workspace_name?: string
  /** Durable run state: 'running' | 'awaiting' (absent = idle). */
  run_status?: 'running' | 'awaiting' | ''
  /** The run behind run_status (the reattach target after a page refresh). */
  run_id?: string
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
/** The selectable provider names (mirrors the server's four). */
export type ProviderName = AppSettings['provider']
export type MemoryBackend = 'none' | 'memory' | 'sqlite' | 'postgres'

export interface AppSettings {
  provider: 'anthropic' | 'openai' | 'glm' | 'deepseek'
  model: string
  max_tokens: number
  temperature: number
  anthropic_base_url: string
  openai_base_url: string
  glm_base_url: string
  deepseek_base_url: string
  anthropic_api_key_set: boolean
  openai_api_key_set: boolean
  glm_api_key_set: boolean
  deepseek_api_key_set: boolean
  checkpoint_backend: CheckpointBackend
  memory_backend: MemoryBackend
  checkpoint_dir: string
  postgres_url_set: boolean
  database: string
  /** Harness observability log (graph/state/session/message traces). */
  debug_log: boolean
  /** Where observability logs live (~/.woca/logs), empty when unwired. */
  log_dir: string
  memory_engine: 'ladym' | 'remote' | 'none'
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
  /** Message plane (run dispatch): pg | pg+nats (live-reloaded). */
  msg_plane_mode: 'pg' | 'pg+nats'
  /** False on personal editions — the plane section stays hidden. */
  msg_plane_available: boolean
  msg_plane_nats_url: string
  msg_plane_subject: string
  model_whitelist: string
  managed_fields?: string[]
}

/** PUT /api/settings body — absent fields are unchanged; empty strings clear. */
export interface AppSettingsUpdate {
  provider?: string
  model?: string
  max_tokens?: number
  temperature?: number
  anthropic_base_url?: string
  openai_base_url?: string
  glm_base_url?: string
  deepseek_base_url?: string
  anthropic_api_key?: string
  openai_api_key?: string
  glm_api_key?: string
  deepseek_api_key?: string
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
  msg_plane_mode?: string
  msg_plane_nats_url?: string
  msg_plane_subject?: string
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

// --- runtime agent controls (mirror /api/agent and /api/quota) ---

export type PermissionMode = 'default' | 'acceptEdits' | 'bypassPermissions' | 'readOnly' | 'plan'
export type ThinkingLevel = 'off' | 'low' | 'medium' | 'high'

/** Live agent state shown next to the chat input. */
export interface AgentState {
  provider: string
  model: string
  permission_mode: PermissionMode
  plan_mode: boolean
  thinking: {
    level: ThinkingLevel
    budget: number
    /** False when the current model has no thinking mode (chip disabled). */
    supported: boolean
    /** Levels the provider template allows, e.g. ['low','medium','high']. */
    levels: ThinkingLevel[]
    /** How the provider maps levels onto its knob (hint shown in the drawer). */
    note?: string
  }
}

/** One quota window from the provider's monitor API. */
export interface QuotaWindow {
  /** Server-defined window label ('5h', 'weekly', 'mcp', …). */
  kind: string
  /** Share of the window already spent (0-100). */
  used_percent: number
  /** Epoch-ms reset time (0 when unknown). */
  next_reset_ms: number
}

export interface QuotaInfo {
  available: boolean
  reason?: string
  source?: string
  /** "plan" (windows) or "balance" (prepaid amount). */
  /** 'plan' (windows) or 'balance' (prepaid). */
  type?: 'plan' | 'balance'
  level?: string
  windows?: QuotaWindow[]
  /** Formatted prepaid amount for balance-type providers (e.g. "CNY 110.35"). */
  balance?: string
}

/** GET /api/quota: provider quota plus this process's session usage. */
export interface QuotaResponse {
  quota: QuotaInfo
  session: {
    requests?: number
    input_tokens?: number
    output_tokens?: number
    cache_read_tokens?: number
    cache_write_tokens?: number
    cost_usd?: number
    cost_known?: boolean
    model?: string
  }
}

// --- context usage (GET /api/sessions/{id}/context) ---

/** One line of the context breakdown (heuristic estimate, not exact). */
export interface ContextCategory {
  /** system | memory | tools | tools_extra | history_summary | history_human | history_ai | history_tool */
  key: string
  label: string
  tokens: number
  /** Messages / files / tools in the category. */
  count?: number
  detail?: string
}

/** Provider-reported usage of the most recent model call (exact values). */
export interface ContextLastUsage {
  input_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  output_tokens: number
}

/** Context-usage snapshot for the chat input's context indicator. */
export interface ContextInfo {
  model: string
  categories: ContextCategory[]
  estimated_total: number
  last_request?: ContextLastUsage
  /** Auto-compact threshold in tokens (0/absent = disabled). */
  compact_threshold?: number
  /** Model context-window limit in tokens (0/absent = unknown). */
  context_window?: number
}

// --- claude environment (mirror /api/claude-env) ---

/** One skill detected under ~/.claude/skills. */
export interface ClaudeSkillEntry {
  name: string
  description: string
  source: string
}

/** One MCP server detected in ~/.claude.json's global mcpServers. */
export interface ClaudeMCPServer {
  name: string
  type: 'http' | 'sse' | 'stdio'
  url?: string
  command?: string
  has_env?: boolean
}

export interface ClaudeEnvState {
  skills: ClaudeSkillEntry[]
  mcp: ClaudeMCPServer[]
  selected: { skills: string[]; mcp: string[] }
}

// --- plugins (mirror /api/plugins) ---

/** One built-in capability plugin (browser / pdf …) with runtime state. */
export interface PluginStatus {
  name: string
  kind: 'golang' | 'python'
  version?: string
  description?: string
  tools: string[]
  runtime?: string
  enabled: boolean
  state: 'not_installed' | 'installing' | 'ready' | 'error'
  detail?: string
  log?: string[]
}

export interface PluginsResponse {
  plugins: PluginStatus[]
}

// --- runtime stats (mirror /api/stats) ---
export interface StatsResponse {
  active_runs: number
  queued_runs: number
  session_queue_depths: Record<string, number>
  teammates: number
  background_tasks: number
  sse_dropped: number
  lsp: { active: number; broken: number }
}
