/// <reference types="vite/client" />

// The vscode-icons collection is a ~3.7MB JSON; letting TypeScript infer
// its full literal shape would explode type-check memory. Only the shape
// we consume is declared here (Vite still inlines the real data at build).
declare module '@iconify-json/vscode-icons/icons.json' {
  const icons: { icons: Record<string, { body: string }> }
  export default icons
}

declare global {
  interface BootstrapState {
    state: 'unconfigured' | 'authorization_required' | 'ready' | 'fatal_local_data'
    server_url?: string
    message?: string
    database_path?: string
    /** Target bridge for edge entries (v1.6 entry ②; empty for direct). */
    bridge_id?: string
    /** The embedded bridge's base URL (wails only; entry ① of the page). */
    embedded_url?: string
  }

  interface ConnectionInput {
    server_url: string
    api_key: string
    bridge_id?: string
  }

  /** One bridge behind an edge access point (pre-auth inventory). */
  interface EdgeBridge {
    bridge_id: string
    created_at: number
    last_seen: number
    live: boolean
  }

  /** Session material for direct SSE connections (native only). */
  interface StreamCredential {
    scheme: string
    token: string
  }

  interface PreferenceInput {
    key: string
    value: string
    revision: number
  }

  interface PreferenceValue {
    value: string
    found: boolean
  }

  interface PreferenceWriteResult {
    revision: number
    applied: boolean
  }

  interface NativeAppBridge {
    Bootstrap(): Promise<BootstrapState>
    Connect(input: ConnectionInput): Promise<BootstrapState>
    Logout(): Promise<BootstrapState>
    ConnectEmbedded(): Promise<BootstrapState>
    EdgeBridges(serverURL: string): Promise<EdgeBridge[]>
    StreamCredentials(serverURL: string): Promise<StreamCredential>
    Preference(key: string): Promise<PreferenceValue>
    SetPreference(input: PreferenceInput): Promise<PreferenceWriteResult>
    CancelRequest(id: string): Promise<boolean>
  }

  interface Window {
    go?: {
      main?: {
        App?: NativeAppBridge
      }
    }
  }
}

export {}
