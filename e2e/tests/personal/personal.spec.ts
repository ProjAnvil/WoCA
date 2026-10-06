import { test, expect } from '@playwright/test';
import { PERSONAL, gate, nativeWizardTestids, pinLocale } from '../../fixtures/lab';

// Form 1 — 个人内嵌 web（`woca serve`，真实二进制 + 隔离 WOCA_HOME）。
// 形态契约：环回信任，无登录门；无原生向导；无 /api/status 能力面。
test.describe('个人内嵌 web（woca serve）', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  test('环回信任：不出现登录门，直接进入应用外壳', async ({ page }) => {
    await page.goto(PERSONAL);

    await expect(page.locator(gate.overlay)).toHaveCount(0);
    await expect(page.locator('aside.sidebar')).toBeVisible();
    await expect(page.locator('.chat-input')).toBeVisible();
  });

  test('反混淆金丝雀：个人 web 不渲染任何原生连接向导卡片', async ({ page }) => {
    await page.goto(PERSONAL);
    await expect(page.locator('aside.sidebar')).toBeVisible();

    for (const tid of nativeWizardTestids) {
      await expect(page.locator(`[data-testid="${tid}"]`)).toHaveCount(0);
    }
    await expect(page.locator('[data-testid="native-loading"]')).toHaveCount(0);
  });

  test('个人版没有 bridge 能力面：侧栏不出现状态灯入口', async ({ page }) => {
    await page.goto(PERSONAL);
    await expect(page.locator('aside.sidebar')).toBeVisible();

    await expect(page.locator('.status-lamp')).toHaveCount(0);
  });

  // 空会话居中问候 → 首条消息后 composer 沉底（FLIP 落定后无残留 transform）。
  test('空会话居中问候：首条消息后 composer 沉底', async ({ page }) => {
    await page.goto(PERSONAL);

    await expect(page.locator('.chat-greeting-text')).toBeVisible();
    await expect(page.locator('.chat-greeting-text')).toContainText(
      /从哪里开始|做点什么|帮你|搭把手|how can I help/i,
    );
    await expect(page.locator('.greeting-chip')).toHaveCount(3);

    // chip 点击 → 填入 composer（fillPrompt 契约）。
    await page.locator('.greeting-chip').first().click();
    await expect(page.locator('.chat-input textarea')).not.toHaveValue('');

    // 发送 → 离开问候态；FLIP 落定（含 400ms 兜底路径）后无残留 transform。
    await page.locator('.chat-input textarea').fill('hello from the greeting test');
    await page.keyboard.press('Enter');
    await expect(page.locator('.chat--greeting')).toHaveCount(0);
    await page.waitForTimeout(600);
    expect(await page.locator('.chat-input').evaluate((el) => getComputedStyle(el).transform)).toBe(
      'none',
    );
  });
});
