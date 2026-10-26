export interface Settings {
  provider: string
  model: string
  max_tokens: number
  temperature: string
  anthropic_base_url: string
  openai_base_url: string
  glm_base_url: string
  deepseek_base_url: string
  checkpoint_backend: string
  memory_backend: string
  checkpoint_dir: string
  database: string
  debug_log: boolean
  memory_engine: string
  ladym_endpoint: string
  ladym_api_username: string
  postgres_url_set: boolean
  checkpoint_pg_host: string
  checkpoint_pg_port: string
  checkpoint_pg_db: string
  checkpoint_pg_user: string
  checkpoint_pg_password_set: boolean
  memory_pg_host: string
  memory_pg_port: string
  memory_pg_db: string
  memory_pg_user: string
  memory_pg_password_set: boolean
  ladym_api_password_set: boolean
  ladym_embedding: string
  ladym_llm: string
  search_providers: string[]
  search_searxng_endpoint: string
  search_custom_endpoint: string
  search_custom_key_set: boolean
  search_allowed_domains: string[]
  search_denied_domains: string[]

  ladym_topk: number
  ladym_autoretain: boolean
  ladym_system2: boolean
  ladym_system2_interval: number
  msg_plane_mode: string
  msg_plane_nats_url: string
  msg_plane_subject: string
  msg_plane_available: boolean
  langfuse_enabled: boolean
  langfuse_base_url: string
  langfuse_public_key: string
  langfuse_secret_key_set: boolean
}

// The backend's execution options (design §17): image, runtime, network,
// cpu/memory caps and env — validated by the backend (runtime only
// runc|runsc, network only none|bridge, cpu/memory >= 0).
export interface SandboxBackendOptions {
  image: string
  runtime: string
  network: string
  cpu: number
  memory_mb: number
  env?: Record<string, string>
}

// One managed sandbox backend (configsvc /api/sandboxes). The API key
// never rides this shape — key_set reports whether one is stored.
export interface SandboxBackend {
  id: string
  name: string
  type: string
  endpoint: string
  timeout_ms: number
  enabled: boolean
  key_set?: boolean
  last_probe_at?: number
  last_probe_ok?: boolean
  last_probe_error?: string
  options?: SandboxBackendOptions
}

// The verify probe's answer: per-step outcomes without credentials.
export interface VerifyResult {
  ok: boolean
  total_latency_ms: number
  steps: Array<{ name: string; ok: boolean; latency_ms: number; error?: string }>
}
