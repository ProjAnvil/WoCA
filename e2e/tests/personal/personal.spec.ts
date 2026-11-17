import { test, expect } from '@playwright/test';
import { PERSONAL, gate, nativeWizardTestids, pinLocale } from '../../fixtures/lab';

// Form 1 — personal embedded web (`woca serve`, the real binary with an
// isolated WOCA_HOME). Form contract: loopback trust with no login gate,
// no native wizard, no /api/status capability surface.
test.describe('personal embedded web (woca serve)', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  test('loopback trust: no login gate, straight into the app shell', async ({ page }) => {
    await page.goto(PERSONAL);

    await expect(page.locator(gate.overlay)).toHaveCount(0);
    await expect(page.locator('aside.sidebar')).toBeVisible();
    await expect(page.locator('.chat-input')).toBeVisible();
  });

  test('anti-confusion canary: personal web renders no native connection-wizard cards', async ({ page }) => {
    await page.goto(PERSONAL);
    await expect(page.locator('aside.sidebar')).toBeVisible();

    for (const tid of nativeWizardTestids) {
      await expect(page.locator(`[data-testid="${tid}"]`)).toHaveCount(0);
    }
    await expect(page.locator('[data-testid="native-loading"]')).toHaveCount(0);
  });

  test('personal has no bridge surface: the sidebar shows no status-lamp entry', async ({ page }) => {
    await page.goto(PERSONAL);
    await expect(page.locator('aside.sidebar')).toBeVisible();

    await expect(page.locator('.status-lamp')).toHaveCount(0);
  });

  // Empty session: centered greeting → after the first message the composer
  // docks to the bottom (FLIP settled, no residual transform).
  test('empty-session greeting: composer docks after the first message', async ({ page }) => {
    await page.goto(PERSONAL);

    await expect(page.locator('.chat-greeting-text')).toBeVisible();
    // The greeting copies are pinned in zh-CN (pinLocale); the English arm
    // covers the en locale fallback.
    await expect(page.locator('.chat-greeting-text')).toContainText(
      /从哪里开始|做点什么|帮你|搭把手|how can I help/i,
    );
    await expect(page.locator('.greeting-chip')).toHaveCount(3);

    // Chip click → fills the composer (the fillPrompt contract). Force-click:
    // the greeting view re-renders when other personal specs create sessions
    // on the shared server mid-test, which detaches the chip between
    // Playwright's stability checks.
    await page.locator('.greeting-chip').first().click({ force: true });
    await expect(page.locator('.chat-input textarea')).not.toHaveValue('');

    // Send → leaves the greeting state; the FLIP settles (including the
    // 400ms fallback path) with no residual transform.
    await page.locator('.chat-input textarea').fill('hello from the greeting test');
    await page.keyboard.press('Enter');
    await expect(page.locator('.chat--greeting')).toHaveCount(0);
    await page.waitForTimeout(600);
    expect(await page.locator('.chat-input').evaluate((el) => getComputedStyle(el).transform)).toBe(
      'none',
    );
  });
});
