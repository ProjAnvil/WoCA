import { test, expect } from '@playwright/test';
import { EDGE, gate, labFacts, nativeWizardTestids, pinLocale } from '../../fixtures/lab';
import { gotoSettingsSection, loginViaGate, openSettings } from '../../fixtures/helpers';

// Form 4 — 企业版，员工用 web browser 经 edge 入口（真实隧道链：
// SPA 与全部 API 经 edge 停靠 → bridge 应答，登录在 edge 应答、核心验证）。
// 形态契约：与直连 bridge 同一门面（同一 SPA、同名 cookie），差异是
// edge 能力面（接入点 bridge 清单）而远端连接区不出现。
test.describe('企业 web browser（edge 入口）', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  test('SPA 经隧道加载：登录门出现在 edge 源', async ({ page }) => {
    await page.goto(EDGE);

    await expect(page.locator(gate.card)).toBeVisible();
    await expect(page.locator(gate.keyInput)).toBeVisible();
  });

  test('edge 登录后经隧道进入应用外壳（API 往返走隧道）', async ({ page }) => {
    const lab = labFacts();
    await loginViaGate(page, EDGE, lab.userKey);

    await expect(page.locator('.chat-input')).toBeVisible();
    // bridge 的 /api/status 经隧道可见 → 状态灯。
    await expect(page.locator('.status-lamp')).toBeVisible();
  });

  test('edge 形态信号：设置面板出现「接入点」并列出已注册 bridge', async ({ page }) => {
    const lab = labFacts();
    await loginViaGate(page, EDGE, lab.userKey);
    await openSettings(page);

    await expect(page.getByText('接入点', { exact: true })).toBeVisible();
    await gotoSettingsSection(page, '接入点');
    const row = page.locator(`[data-testid^="edge-bridge-"]`).first();
    await expect(row).toBeVisible();
    // 行标识就是 lab 注册的 bridge。
    await expect(page.locator(`[data-testid="edge-bridge-${lab.bridgeID}"]`)).toBeVisible();
    // edge 源不出现 bridge 专属的「远端连接」。
    await expect(page.getByText('远端连接')).toHaveCount(0);
  });

  test('反混淆金丝雀：edge web 不渲染任何原生连接向导卡片', async ({ page }) => {
    const lab = labFacts();
    await loginViaGate(page, EDGE, lab.userKey);

    for (const tid of nativeWizardTestids) {
      await expect(page.locator(`[data-testid="${tid}"]`)).toHaveCount(0);
    }
  });
});
