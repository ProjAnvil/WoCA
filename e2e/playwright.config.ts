import { defineConfig } from '@playwright/test';
import { repoRoot } from './fixtures/paths';

// Fixed loopback ports for the four forms under test. The enterprise lab
// (fake core + real edge + real bridge) writes its runtime facts (minted
// user key, bridge id) to .runtime/lab.json once every listener is healthy.
const PORTS = {
  personal: 13710, // real `woca serve` binary (personal embedded web)
  bridge: 13711, // lab enterprise topology: bridge direct listener
  edge: 13712, // lab enterprise topology: edge public entry
  static: 13714, // lab static topology: webdist for the native-wizard shim
  relay: 13715, // lab relay topology: woca-relay public entry
  relayWoca: 13716, // lab relay topology: the personal serve behind it
};
const base = (p: number) => `http://127.0.0.1:${p}`;
const RUNTIME = `${repoRoot}/e2e/.runtime`;

process.env.WOCA_E2E_PERSONAL ??= base(PORTS.personal);
process.env.WOCA_E2E_BRIDGE ??= base(PORTS.bridge);
process.env.WOCA_E2E_EDGE ??= base(PORTS.edge);
process.env.WOCA_E2E_STATIC ??= base(PORTS.static);
process.env.WOCA_E2E_RELAY ??= base(PORTS.relay);

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 4,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    trace: 'on-first-retry',
    locale: 'zh-CN',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  webServer: [
    {
      // Form 1 — personal embedded web: the real binary, isolated state.
      // Built inline (Playwright starts web servers before globalSetup).
      // HOME isolation is applied only to the exec'd woca process — the
      // build step must keep the real module cache (Go makes mod-cache
      // files read-only, which would break the next run's wipe).
      command: [
        'mkdir -p e2e/.bin',
        'go build -o e2e/.bin/woca ./cmd/woca',
        'rm -rf e2e/.runtime/personal-home',
        'mkdir -p e2e/.runtime/personal-home',
        `exec env HOME=${RUNTIME}/personal-home WOCA_HOME=${RUNTIME}/personal-home WOCA_DB=${RUNTIME}/personal-home/woca.db WOCA_PLUGIN_ROOT=${RUNTIME}/personal-home/plugins WOCA_NO_BROWSER=1 ./e2e/.bin/woca serve --addr 127.0.0.1:${PORTS.personal} --no-browser --log-level warn`,
      ].join(' && '),
      url: `${base(PORTS.personal)}/api/health`,
      cwd: repoRoot,
      env: { ...process.env },
      reuseExistingServer: false,
      timeout: 120_000,
      stdout: 'ignore',
      stderr: 'pipe',
    },
    {
      // Forms 2 & 4 — one process hosting the real private-core chain:
      // fake core (real stores) + real edge handler + real bridge (enrolled
      // over the real /gw/enroll flow, apikey posture, tunnel open).
      command: 'go run ./e2e/lab -topology enterprise',
      url: `${base(PORTS.bridge)}/api/health`,
      cwd: repoRoot,
      env: { ...process.env, WOCA_NO_BROWSER: '1' },
      reuseExistingServer: false,
      timeout: 180_000,
      stdout: 'pipe',
      stderr: 'pipe',
    },
    {
      // Form 3 — the built webdist served plain, for the native-wizard
      // tests (window.go shim injected per-test).
      command: 'go run ./e2e/lab -topology static',
      url: `${base(PORTS.static)}/__e2e/health`,
      cwd: repoRoot,
      env: { ...process.env },
      reuseExistingServer: false,
      timeout: 120_000,
      stdout: 'ignore',
      stderr: 'pipe',
    },
    {
      // Form 5 — personal remote relay: real woca-relay (keys + auth +
      // tunnel + SPA) and the real personal serve behind it, the tunnel
      // verified live before readiness is declared.
      command: 'go run ./e2e/lab -topology relay',
      url: `${base(PORTS.relay)}/api/health`,
      cwd: repoRoot,
      env: { ...process.env, WOCA_NO_BROWSER: '1' },
      reuseExistingServer: false,
      timeout: 180_000,
      stdout: 'pipe',
      stderr: 'pipe',
    },
    // Opt-in Form 3 upgrade — the REAL wails process (`wails dev` serves
    // the app with Go bindings to a plain browser at :34115). Opens a
    // desktop window and needs the wails CLI, so it never runs by
    // default: WOCA_E2E_NATIVE_DEV=1 npx playwright test.
    ...(process.env.WOCA_E2E_NATIVE_DEV === '1'
      ? [
          {
            command: [
              'rm -rf e2e/.runtime/wails-home',
              'go build -o build/woca-bridge ./cmd/woca-bridge',
              'cp build/woca-bridge native-client/bridgebin/woca-bridge',
              'cd native-client && exec env WOCA_HOME=' +
                `${RUNTIME}/wails-home` +
                ' WOCA_NO_BROWSER=1 wails dev -tags embeddedbridge',
            ].join(' && '),
            url: 'http://127.0.0.1:34115',
            cwd: repoRoot,
            env: { ...process.env },
            reuseExistingServer: true,
            timeout: 300_000,
            stdout: 'ignore' as const,
            stderr: 'pipe' as const,
          },
        ]
      : []),
  ],
});
