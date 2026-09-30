import { test, expect } from '@playwright/test';
import { BRIDGE, gate, labFacts, nativeWizardTestids, pinLocale } from '../../fixtures/lab';
import { loginViaGate, openSettings } from '../../fixtures/helpers';

// Form 2 — 企业 bridge 端 web（真实 bridge：apikey 姿态、已注册 lab 私有核心）。
// 形态契约：启动 401 → LoginGate；登录在核心验证；bridge 能力面（状态灯、
// 组织配置、远端连接）；绝不出现原生向导卡片。
test.describe('企业 bridge web（直连 bridge）', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  test('未登录：出现 API-Key 登录门（含 QR 配对入口）', async ({ page }) => {
    await page.goto(BRIDGE);

    await expect(page.locator(gate.overlay)).toBeVisible();
    await expect(page.locator(gate.card)).toBeVisible();
    await expect(page.locator(gate.keyInput)).toBeVisible();
    await expect(page.locator(gate.submit)).toBeVisible();
    await expect(page.locator(gate.qr)).toBeVisible();
    // 登录门是覆盖层：外壳 DOM 在其下渲染属设计行为，但 overlay 必须
    // 铺满视口（用户在登录前接触不到应用）。
    const box = await page.locator(gate.overlay).boundingBox();
    expect(box).toBeTruthy();
    expect(box!.width).toBeGreaterThanOrEqual(page.viewportSize()!.width - 1);
    expect(box!.height).toBeGreaterThanOrEqual(page.viewportSize()!.height - 1);
  });

  test('错误密钥：展示错误且登录门保持', async ({ page }) => {
    await page.goto(BRIDGE);
    await expect(page.locator(gate.card)).toBeVisible();

    await page.locator(gate.keyInput).fill('woca_definitely_wrong_key');
    await page.locator(gate.submit).click();

    await expect(page.locator(gate.error)).toBeVisible();
    await expect(page.locator(gate.card)).toBeVisible();
  });

  test('反混淆金丝雀：bridge web 不渲染任何原生连接向导卡片', async ({ page }) => {
    await page.goto(BRIDGE);
    await expect(page.locator(gate.card)).toBeVisible();

    for (const tid of nativeWizardTestids) {
      await expect(page.locator(`[data-testid="${tid}"]`)).toHaveCount(0);
    }
  });

  test('正确密钥：进入应用外壳，bridge 能力面齐备', async ({ page }) => {
    const lab = labFacts();
    await loginViaGate(page, BRIDGE, lab.userKey);

    await expect(page.locator('.chat-input')).toBeVisible();
    // /api/status 是 JSON（edition=bridge）→ 侧栏状态灯出现。
    await expect(page.locator('.status-lamp')).toBeVisible();

    // 直连 bridge 的形态信号：设置面板有「组织配置」（bridge 专属）与
    // 「远端连接」（bridge 保存的远端清单），但没有 edge 专属的「接入点」。
    await openSettings(page);
    await expect(page.getByText('组织配置')).toBeVisible();
    await expect(page.getByText('远端连接')).toBeVisible();
    await expect(page.getByText('接入点', { exact: true })).toHaveCount(0);
  });
});
