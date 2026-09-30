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
});
