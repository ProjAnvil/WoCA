import { test, expect } from '@playwright/test';
import { RELAY, gate, nativeWizardTestids, pinLocale, relayFacts } from '../../fixtures/lab';
import { loginViaGate } from '../../fixtures/helpers';

// Form 5 — 个人远程 relay（woca-relay + 个人 woca serve，真实链路：
// 浏览器打 relay 源，SPA 由 relay 服务，登录用 wcr_ 会合密钥，全部
// /api/* 经出站隧道回到个人 serve）。
// 形态契约：出现通用 LoginGate（这是个人版唯一会出现登录门的形态），
// 但门后是个人版能力面：无状态灯、无组织配置；绝不出现原生向导卡片。
test.describe('个人远程 relay（wcr_ 密钥登录）', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  test('未登录：relay 源出现 API-Key 登录门', async ({ page }) => {
    await page.goto(RELAY);

    await expect(page.locator(gate.overlay)).toBeVisible();
    await expect(page.locator(gate.card)).toBeVisible();
    await expect(page.locator(gate.keyInput)).toBeVisible();
    await expect(page.locator(gate.submit)).toBeVisible();
  });

  test('错误密钥：展示错误且登录门保持', async ({ page }) => {
    await page.goto(RELAY);
    await expect(page.locator(gate.card)).toBeVisible();

    await page.locator(gate.keyInput).fill('wcr_definitely_wrong_key');
    await page.locator(gate.submit).click();

    await expect(page.locator(gate.error)).toBeVisible();
    await expect(page.locator(gate.card)).toBeVisible();
  });

  test('吊销密钥：登录门展示吊销通告（revoked-notice）', async ({ page }) => {
    const facts = relayFacts();
    await page.goto(RELAY);
    await expect(page.locator(gate.card)).toBeVisible();

    await page.locator(gate.keyInput).fill(facts.revokedKey);
    // 等登录往返完成再断言：负向断言在异步动作后会即刻通过（竞态误绿），
    // 必须先锚定响应已回来。
    const loginDone = page.waitForResponse((r) => r.url().includes('/api/auth/login'));
    await page.locator(gate.submit).click();
    await loginDone;

    await expect(page.locator('[data-testid="revoked-notice"]')).toBeVisible();
    await expect(page.locator(gate.card)).toBeVisible();
  });

  test('正确 wcr_ 密钥：经隧道进入个人版应用外壳', async ({ page }) => {
    const facts = relayFacts();
    await loginViaGate(page, RELAY, facts.relayKey);

    await expect(page.locator('.chat-input')).toBeVisible();
    // 个人版能力面：/api/status 不是 JSON（serve 的 SPA 面）→ 无状态灯。
    await expect(page.locator('.status-lamp')).toHaveCount(0);
  });

  test('形态信号：设置面板不出现组织配置（个人远程非 bridge 形态）', async ({ page }) => {
    const facts = relayFacts();
    await loginViaGate(page, RELAY, facts.relayKey);
    await page.getByRole('button', { name: '设置' }).click();
    await expect(page.locator('.settings-nav')).toBeVisible();

    await expect(page.getByText('组织配置')).toHaveCount(0);
    await expect(page.getByText('接入点', { exact: true })).toHaveCount(0);
  });

  test('反混淆金丝雀：relay web 不渲染任何原生连接向导卡片', async ({ page }) => {
    await page.goto(RELAY);
    await expect(page.locator(gate.card)).toBeVisible();

    for (const tid of nativeWizardTestids) {
      await expect(page.locator(`[data-testid="${tid}"]`)).toHaveCount(0);
    }
  });
});
