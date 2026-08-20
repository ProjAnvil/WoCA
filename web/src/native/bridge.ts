export interface NativeBridge {
  Bootstrap(): Promise<BootstrapState>
  Connect(input: ConnectionInput): Promise<BootstrapState>
  Logout(): Promise<BootstrapState>
  Preference(key: string): Promise<PreferenceValue>
  PutPreference(input: PreferenceInput): Promise<void>
}

export function getNativeBridge(): NativeBridge | null {
  return window.go?.main?.App ?? null
}
