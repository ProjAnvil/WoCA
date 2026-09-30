import type { Page } from '@playwright/test';

// A test double of the wails Go bindings (window.go.main.App) implementing
// the NativeBridge contract faithfully (web/src/native/bridge.ts) with a
// scriptable scenario. The wizard under test is the REAL ConnectView/
// NativeGate component tree from the built webdist — only the wails
// process boundary is substituted, which is exactly the boundary a
// browser cannot host (Wails v2 has no headless mode).
export type ShimConfig = {
  /** The embedded bridge origin; omit to simulate a remote-only build. */
  embeddedUrl?: string;
  /** Saved connections the wizard lists (Profiles()). */
  profiles?: Array<{
    id: number;
    server_url: string;
    bridge_id?: string;
    session_state: 'active' | 'expired' | 'signed_out';
    last_used_at: number;
    active: boolean;
  }>;
  /** Pre-auth edge inventory (EdgeBridges()). Two entries force the P3
   * bridge picker; one auto-connects. */
  bridges?: Array<{ bridge_id: string; live: boolean }>;
  /** What EnrollEmbedded returns (error class token; 'user_key' routes
   * the wizard to the P5 single-key page). */
  enrollError?: string;
  /** The embedded bridge's cloud-enrollment posture (controls whether the
   * wizard offers the register / deregister entries on P1). */
  enrolled?: boolean;
};

export async function installNativeShim(page: Page, cfg: ShimConfig = {}): Promise<void> {
  await page.addInitScript((config: ShimConfig) => {
    const calls: Array<[string, unknown]> = [];
    (window as unknown as { __nativeCalls: Array<[string, unknown]> }).__nativeCalls = calls;
    const record = (name: string, input?: unknown) => calls.push([name, input ?? null]);
    const bootstrap = { state: 'unconfigured' as const, embedded_url: config.embeddedUrl ?? '' };
    const app = {
      Bootstrap: async () => {
        record('Bootstrap');
        return { ...bootstrap };
      },
      Connect: async (input: unknown) => {
        record('Connect', input);
        return { state: 'ready', server_url: (input as { server_url: string }).server_url, bridge_id: (input as { bridge_id?: string }).bridge_id ?? '' };
      },
      Logout: async () => {
        record('Logout');
        return { state: 'unconfigured' };
      },
      ConnectEmbedded: async () => {
        record('ConnectEmbedded');
        return { state: 'ready', server_url: config.embeddedUrl ?? 'http://127.0.0.1:1' };
      },
      EdgeBridges: async (serverURL: string) => {
        record('EdgeBridges', serverURL);
        return (config.bridges ?? []).map((b, i) => ({
          bridge_id: b.bridge_id,
          created_at: 1,
          last_seen: Date.now(),
          live: b.live,
          _i: i,
        }));
      },
      StreamCredentials: async (serverURL: string) => {
        record('StreamCredentials', serverURL);
        return { scheme: 'Bearer', token: 'shim-token' };
      },
      Preference: async (key: string) => {
        record('Preference', key);
        return { value: '', found: false };
      },
      SetPreference: async (input: unknown) => {
        record('SetPreference', input);
        return { revision: 1, applied: true };
      },
      CancelRequest: async (id: string) => {
        record('CancelRequest', id);
        return true;
      },
      Profiles: async () => {
        record('Profiles');
        return config.profiles ?? [];
      },
      ResumeProfile: async (id: number) => {
        record('ResumeProfile', id);
        return { state: 'ready', server_url: 'http://127.0.0.1:1' };
      },
      DeleteProfile: async (id: number) => {
        record('DeleteProfile', id);
        return { deleted: true, had_session: false, revoked: true };
      },
      EmbeddedCloudStatus: async () => {
        record('EmbeddedCloudStatus');
        return {
          available: Boolean(config.embeddedUrl),
          enrolled: config.enrolled ?? false,
          bridge_id: config.embeddedUrl ? 'br_emb' : undefined,
          edge_url: config.enrolled ? 'http://127.0.0.1:1' : undefined,
        };
      },
      EnrollEmbedded: async (input: unknown) => {
        record('EnrollEmbedded', input);
        return { state: { state: 'ready' }, bridge_id: 'br_emb', error: config.enrollError ?? '' };
      },
      DeregisterEmbedded: async () => {
        record('DeregisterEmbedded');
        return { error: '' };
      },
    };
    (window as unknown as { go: unknown }).go = { main: { App: app } };
  }, cfg);
}

/** The recorded native binding calls: [[name, input], ...]. */
export function nativeCalls(page: Page): Promise<Array<[string, unknown]>> {
  return page.evaluate(() =>
    (window as unknown as { __nativeCalls: Array<[string, unknown]> }).__nativeCalls,
  );
}
