import fs from 'node:fs';
import { labFactsPath } from './paths';

export type LabFacts = {
  /** Direct bridge origin (form 2), e.g. http://127.0.0.1:13711 */
  bridgeBase: string;
  /** Edge public entry origin (form 4), e.g. http://127.0.0.1:13712 */
  edgeBase: string;
  /** A valid woca_ tenant-admin key minted on the lab core. */
  userKey: string;
  /** The enrolled bridge id as the edge inventory reports it. */
  bridgeID: string;
};

export type RelayFacts = {
  /** Relay public origin (form 5), e.g. http://127.0.0.1:13715 */
  relayBase: string;
  /** The personal serve's local origin behind the relay. */
  wocaBase: string;
  /** An active wcr_ rendezvous key. */
  relayKey: string;
  /** A wcr_ key that was revoked before the lab reported ready. */
  revokedKey: string;
};

/** Runtime facts of the enterprise lab (written once all listeners are healthy). */
export function labFacts(): LabFacts {
  return JSON.parse(fs.readFileSync(labFactsPath, 'utf8')) as LabFacts;
}

/** Runtime facts of the relay lab (written once the tunnel is verified live). */
export function relayFacts(): RelayFacts {
  return JSON.parse(
    fs.readFileSync(labFactsPath.replace('lab.json', 'relay.json'), 'utf8'),
  ) as RelayFacts;
}

export const PERSONAL = process.env.WOCA_E2E_PERSONAL!;
export const BRIDGE = process.env.WOCA_E2E_BRIDGE!;
export const EDGE = process.env.WOCA_E2E_EDGE!;
export const STATIC = process.env.WOCA_E2E_STATIC!;
export const RELAY = process.env.WOCA_E2E_RELAY!;

/** Pin the UI locale so text assertions stay deterministic. */
export async function pinLocale(page: import('@playwright/test').Page): Promise<void> {
  await page.addInitScript(() => localStorage.setItem('woca.pref.locale', 'zh-CN'));
}

/** Selector helpers for the two connection surfaces under test. */
export const gate = {
  overlay: '.login-gate-overlay',
  card: '.login-gate',
  keyInput: '[data-testid="login-key-input"], .login-gate-row input[type="password"]',
  submit: '[data-testid="login-submit"], .login-submit',
  error: '.login-gate-error',
  qr: '.login-gate-qr',
};

/** Every native-wizard card that must NEVER appear in a browser form. */
export const nativeWizardTestids = [
  'saved-profiles',
  'embedded-entry',
  'embedded-connect',
  'embedded-signin',
  'remote-open',
  'tab-private',
  'tab-direct',
  'bridge-step',
  'enroll-step',
  'enroll-login-step',
] as const;
