export interface NativeBridge {
  Bootstrap(): Promise<BootstrapState>
  Connect(input: ConnectionInput): Promise<BootstrapState>
  Logout(): Promise<BootstrapState>
  Preference(key: string): Promise<PreferenceValue>
  SetPreference(input: PreferenceInput): Promise<PreferenceWriteResult>
  CancelRequest(id: string): Promise<boolean>
}

export function getNativeBridge(): NativeBridge | null {
  return window.go?.main?.App ?? null
}
