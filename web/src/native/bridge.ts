export interface NativeBridge {
  Bootstrap(): Promise<BootstrapState>
  Connect(input: ConnectionInput): Promise<BootstrapState>
  Logout(): Promise<BootstrapState>
}

export function getNativeBridge(): NativeBridge | null {
  return window.go?.main?.App ?? null
}
