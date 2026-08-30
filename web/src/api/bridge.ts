// The bridge product surface: login/logout, status, cloud access, the
// edge access-point probe and inventory, and the read-only admin views.
import i18n from '../i18n'
import { postJSON, request } from './transport'

// --- login / identity ---

export interface LoginResponse {
  token: string
  expires_at: number
  tenant_id: string
  user_id: string
  role: string
}

export function login(apiKey: string): Promise<LoginResponse> {
  return postJSON('/api/auth/login', { api_key: apiKey })
}

export function logout(): Promise<void> {
  return postJSON('/api/auth/logout', {})
}

// --- status ---

export interface GatewayStatus {
  enrolled: boolean
  bridge_id?: string
  last_version?: string
  last_pull?: string
  stale: boolean
}

export interface TunnelStatus {
  serving: boolean
  served: number
}

export interface CloudAccessStatus {
  url: string
  url_source: string
  token_source: string
  token_set: boolean
  enrolled: boolean
  bridge_id?: string
  last_version?: string
  last_pull?: string
  stale: boolean
  spki_pin?: string
  spki_pinned: boolean
  tunnel: TunnelStatus
}

export interface BridgeStatus {
  edition: string
  /** Auth posture: "local" (loopback trust) or "apikey". */
  auth?: string
  /** Cloud-channel snapshot; absent on editions without the channel. */
  cloud?: CloudAccessStatus
  managed: boolean
}

export function getStatus(): Promise<BridgeStatus> {
  return request('/api/status', undefined, i18n.global.t('api.statusFailed'))
}

// --- bridge cloud access (design §6.5: the bridge-local config layer) ---

export function getCloudAccess(): Promise<CloudAccessStatus> {
  return request('/api/cloud-access', undefined, i18n.global.t('api.requestFailed'))
}

export function updateCloudAccess(url: string, enrollmentToken: string): Promise<CloudAccessStatus> {
  return request(
    '/api/cloud-access',
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, enrollment_token: enrollmentToken }),
    },
    i18n.global.t('api.requestFailed'),
  )
}

export function resetCloudAccess(): Promise<CloudAccessStatus> {
  return request(
    '/api/cloud-access/reset',
    { method: 'POST' },
    i18n.global.t('api.requestFailed'),
  )
}

// --- edge access point (scenario ④: browser bridge switching) ---

export interface EdgeBridge {
  bridge_id: string
  created_at: number
  last_seen: number
  live: boolean
}

/**
 * Probes the same-origin edge inventory (GET /api/edge/bridges — the
 * edge's own pre-auth route, never parked into a tunnel). Returns null
 * unless the origin really is an edge access point: direct-bridge and
 * personal deployments answer an unknown /api path with the SPA shell
 * (200 HTML), which must not pass for an inventory.
 */
export async function getEdgeBridges(): Promise<EdgeBridge[] | null> {
  try {
    const resp = await fetch('/api/edge/bridges', {
      headers: { Accept: 'application/json' },
    })
    if (!resp.ok) return null
    const ct = resp.headers.get('Content-Type') ?? ''
    if (!ct.includes('application/json')) return null
    const data = (await resp.json()) as unknown
    if (!Array.isArray(data)) return null
    const out: EdgeBridge[] = []
    for (const item of data) {
      const b = item as Partial<EdgeBridge>
      if (typeof b.bridge_id !== 'string' || !b.bridge_id) continue
      out.push({
        bridge_id: b.bridge_id,
        created_at: typeof b.created_at === 'number' ? b.created_at : 0,
        last_seen: typeof b.last_seen === 'number' ? b.last_seen : 0,
        live: b.live === true,
      })
    }
    return out
  } catch {
    return null
  }
}

// --- bridge admin surface ---

export interface Me {
  user_id: string
  remark: string
  role: string
}

export function getMe(): Promise<Me> {
  return request('/api/auth/me', undefined, i18n.global.t('api.requestFailed'))
}

export interface BridgeSession {
  key_prefix: string
  remark: string
  role: string
  created_at: number
  last_seen: number
  expires_at: number
}

export function listBridgeSessions(): Promise<BridgeSession[]> {
  return request('/api/sessions/admin', undefined, i18n.global.t('api.requestFailed'))
}

export interface AuditRow {
  at: number
  actor: string
  action: string
  detail: string
}

export function listAudit(limit = 100): Promise<AuditRow[]> {
  return request(`/api/audit?limit=${limit}`, undefined, i18n.global.t('api.requestFailed'))
}

// --- remote profiles (bridge 形态的接入点切换: saved remote origins in
// the bridge's settings DB; switching is a full navigation, the target's
// own login gate takes over) ---

export interface WebProfile {
  id: string
  url: string
  label?: string
  added_at: number
}

export interface WebProfiles {
  profiles: WebProfile[]
  last?: string
}

export function getWebProfiles(): Promise<WebProfiles> {
  return request('/api/profiles', undefined, i18n.global.t('api.requestFailed'))
}

export function addWebProfile(url: string, label = ''): Promise<WebProfile> {
  return postJSON('/api/profiles', { url, label }, i18n.global.t('api.requestFailed'))
}

export function useWebProfile(id: string): Promise<WebProfile> {
  return postJSON(`/api/profiles/${encodeURIComponent(id)}/use`, {}, i18n.global.t('api.requestFailed'))
}

export function deleteWebProfile(id: string): Promise<WebProfiles> {
  return request(
    `/api/profiles/${encodeURIComponent(id)}`,
    { method: 'DELETE' },
    i18n.global.t('api.requestFailed'),
  )
}
