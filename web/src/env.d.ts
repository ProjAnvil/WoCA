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
    state: 'unconfigured' | 'authorization_required' | 'ready'
    server_url?: string
  }

  interface ConnectionInput {
    server_url: string
    api_key: string
  }

  interface PreferenceInput {
    key: string
    value: string
  }

  interface PreferenceValue {
    value: string
    found: boolean
  }

  interface NativeAppBridge {
    Bootstrap(): Promise<BootstrapState>
    Connect(input: ConnectionInput): Promise<BootstrapState>
    Logout(): Promise<BootstrapState>
    Preference(key: string): Promise<PreferenceValue>
    PutPreference(input: PreferenceInput): Promise<void>
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
