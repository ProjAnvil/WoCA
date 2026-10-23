// The settings page's REST surface: server settings, the claude
// environment import, built-in plugins, the model catalog, and the
// config-plane managed delivery.
import i18n from '../i18n'
import type {
  AppSettings,
  AppSettingsUpdate,
  ClaudeEnvState,
  PluginsResponse,
  WebSearchState,
  WebSearchTestResult,
  WebSearchUpdate,
} from '../types'
import { postJSON, request } from './transport'

export function getSettings(): Promise<AppSettings> {
  return request('/api/settings', undefined, i18n.global.t('api.loadSettingsFailed'))
}

export function updateSettings(
  patch: AppSettingsUpdate,
): Promise<{ restart_required: boolean; apply_error?: string }> {
  return request('/api/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  }, i18n.global.t('api.saveSettingsFailed'))
}

// --- claude environment (Skill / MCP checkboxes) ---

/** Lists ~/.claude skills + global MCP servers and the current selection. */
export function getClaudeEnv(): Promise<ClaudeEnvState> {
  return request('/api/claude-env', undefined, i18n.global.t('api.loadClaudeEnvFailed'))
}

/** Saves the checkbox selection; skills apply live, MCP at next agent start. */
export function putClaudeEnv(body: {
  skills: string[]
  mcp: string[]
}): Promise<{ ok: boolean; mcp_restart: boolean }> {
  return request('/api/claude-env', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }, i18n.global.t('api.saveFailed'))
}

// --- plugins (settings panel section) ---

/** Lists built-in capability plugins with enabled + runtime install state. */
export function getPlugins(): Promise<PluginsResponse> {
  return request('/api/plugins', undefined, i18n.global.t('api.loadPluginsFailed'))
}

/** Persists the enabled plugin selection; the agent tool set rebinds live. */
export function putPlugins(enabled: string[]): Promise<{ ok: boolean }> {
  return request('/api/plugins', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  }, i18n.global.t('api.saveFailed'))
}

/** Starts an async runtime install; poll getPlugins() for progress. */
export function installPlugin(name: string, reinstall = false): Promise<{ ok: boolean }> {
  return request(
    `/api/plugins/${encodeURIComponent(name)}/${reinstall ? 'reinstall' : 'install'}`,
    { method: 'POST' },
    i18n.global.t('api.installStartFailed'),
  )
}

// --- model catalog (private-cloud form: pulled via the bridge) ---

export interface ModelPreset {
  id: string
  name: string
  provider: string
  model: string
  max_tokens?: number
  temperature?: number
  base_url?: string
}

export function listModels(): Promise<ModelPreset[]> {
  return request('/api/models', undefined, i18n.global.t('api.requestFailed'))
}

export function applyModel(id: string): Promise<{ ok: boolean; provider: string; model: string; apply_error?: string }> {
  return postJSON(`/api/models/${encodeURIComponent(id)}/apply`, {})
}

// --- managed delivery (远端配置: config-plane skills & MCP) ---

export interface ManagedSkill {
  name: string
  description?: string
}

export interface ManagedMCPServer {
  name: string
  type: string
  url?: string
  command?: string
}

export interface ManagedDelivery {
  skills: ManagedSkill[]
  mcp: ManagedMCPServer[]
}

/**
 * The operator-delivered skill/MCP set (read-only view of the managed
 * layer's materialized files; env values never cross the wire).
 */
export function getManagedDelivery(): Promise<ManagedDelivery> {
  return request('/api/managed', undefined, i18n.global.t('api.requestFailed'))
}

// --- web search ---

/** Lists the backend catalog and the current failover order. */
export function getWebSearch(): Promise<WebSearchState> {
  return request('/api/websearch', undefined, i18n.global.t('api.loadWebSearchFailed'))
}

/** Replaces the whole search block; the chain rebinds on the next turn. */
export function putWebSearch(body: WebSearchUpdate): Promise<{ ok: boolean }> {
  return request('/api/websearch', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }, i18n.global.t('api.saveWebSearchFailed'))
}

/** Runs one real query so the user can confirm the configuration works. */
export function testWebSearch(query: string): Promise<WebSearchTestResult> {
  return request('/api/websearch/test', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ query }),
  }, i18n.global.t('api.testWebSearchFailed'))
}
