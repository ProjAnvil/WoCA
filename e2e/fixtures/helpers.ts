import type { Page } from '@playwright/test';
import { expect } from '@playwright/test';
import { gate, pinLocale } from './lab';

/** Open a gated origin and log in through the API-key gate card. */
export async function loginViaGate(page: Page, origin: string, key: string): Promise<void> {
  await pinLocale(page);
  await page.goto(origin);
  await expect(page.locator(gate.card)).toBeVisible();
  await page.locator(gate.keyInput).fill(key);
  await page.locator(gate.submit).click();
  // The SPA reloads itself on auth; the app shell appears after.
  await expect(page.locator('aside.sidebar')).toBeVisible({ timeout: 30_000 });
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
