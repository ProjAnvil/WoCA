import type { Page } from '@playwright/test';
import { expect } from '@playwright/test';
import { gate, pinLocale } from './lab';

/** Open a gated origin and log in through the API-key gate card.
 * Completion must be anchored across the SPA's post-auth self-reload:
 * the pre-reload page already renders the shell under the gate overlay,
 * so "sidebar visible" is NOT a valid login-complete signal — wait for
 * the login response, then for the gate to be gone on the reloaded
 * page. */
export async function loginViaGate(page: Page, origin: string, key: string): Promise<void> {
  await pinLocale(page);
  await page.goto(origin);
  await expect(page.locator(gate.card)).toBeVisible();
  await page.locator(gate.keyInput).fill(key);
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.url().includes('/api/auth/login')),
    page.locator(gate.submit).click(),
  ]);
  if (response.status() !== 200) {
    throw new Error(`login responded ${response.status()}, expected 200`);
  }
  // The SPA reloads itself ~380ms after auth; the gate stays open until
  // then, so waiting for it to disappear crosses the navigation.
  await expect(page.locator(gate.card)).toBeHidden({ timeout: 30_000 });
  await expect(page.locator('aside.sidebar')).toBeVisible();
}

/** Open the sidebar settings panel (the footer 设置 button — the footer's
 * 状态 button shares its class, so select by accessible name). */
export async function openSettings(page: Page): Promise<void> {
  await page.getByRole('button', { name: '设置' }).click();
  await expect(page.locator('.settings-nav')).toBeVisible();
}

/** Activate a settings nav section by its zh label and wait for its pane.
 * The panel's motion design keeps nav items animating (never "stable"),
 * so the click bypasses the stability check. */
export async function gotoSettingsSection(page: Page, label: string): Promise<void> {
  await page.locator('.settings-nav-item', { hasText: label }).click({ force: true });
}
