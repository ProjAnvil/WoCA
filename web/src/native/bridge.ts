// One saved connection on the connect page (the one-tap resume list).
// Session states: "active" (stored session, can re-enter directly),
// "expired" (session past its expiry), "signed_out" (no session stored).
export interface ProfileSummary {
  id: number
  server_url: string
  bridge_id?: string
  session_state: 'active' | 'expired' | 'signed_out'
  last_used_at: number
  active: boolean
}

// What a DeleteProfile did: local records are gone; when a live session
// existed, whether the server-side revocation succeeded (best effort).
export interface ProfileDeletion {
  deleted: boolean
  had_session: boolean
  revoked: boolean
}

// The embedded bridge's private-cloud enrollment state (the connect
// page's embedded entry). Enrolled comes from the app's posture marker —
// an apikey bridge's status API is not anonymous-readable.
export interface EmbeddedCloudStatus {
  available: boolean
  enrolled: boolean
  bridge_id?: string
  edge_url?: string
}

// The three-field enrollment form: machine-plane registration (edge +
// one-time enrollment token) followed by the user-plane login (API key).
export interface EnrollEmbeddedInput {
  edge_url: string
  enrollment_token: string
  api_key: string
}

// Enrollment outcome: error is a class token ("" on success) — a rejected
// key still leaves the machine enrolled, so the flow never rejects.
export type EnrollErrorClass =
  | ''
  | 'edge_unreachable'
  | 'bridge_write'
  | 'enroll_token'
  | 'bridge_restart'
  | 'user_key'
  | 'enroll_unavailable'

export interface EnrollResult {
  state?: BootstrapState
  bridge_id?: string
  error?: EnrollErrorClass
}

export interface NativeBridge {
  Bootstrap(): Promise<BootstrapState>
  Connect(input: ConnectionInput): Promise<BootstrapState>
  Logout(): Promise<BootstrapState>
  ConnectEmbedded(): Promise<BootstrapState>
  EdgeBridges(serverURL: string): Promise<EdgeBridge[]>
  StreamCredentials(serverURL: string): Promise<StreamCredential>
  Preference(key: string): Promise<PreferenceValue>
  SetPreference(input: PreferenceInput): Promise<PreferenceWriteResult>
  CancelRequest(id: string): Promise<boolean>
  // Saved-connection management (optional: older binaries predate it).
  Profiles?(): Promise<ProfileSummary[]>
  ResumeProfile?(id: number): Promise<BootstrapState>
  DeleteProfile?(id: number): Promise<ProfileDeletion>
  // Embedded-bridge enrollment (optional: dev builds without the bundled
  // binary, older binaries).
  EmbeddedCloudStatus?(): Promise<EmbeddedCloudStatus>
  EnrollEmbedded?(input: EnrollEmbeddedInput): Promise<EnrollResult>
  DeregisterEmbedded?(): Promise<EnrollResult>
}

export function getNativeBridge(): NativeBridge | null {
  return window.go?.main?.App ?? null
}
