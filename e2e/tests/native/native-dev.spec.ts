import { test, expect } from '@playwright/test';
import { gate, pinLocale } from '../../fixtures/lab';

// Form 3（真机层，opt-in）— 真实 wails 进程：`wails dev` 把含 Go 绑定的
// 完整应用提供给浏览器（http://127.0.0.1:34115），因此这里驱动的是真
// NativeGate/真嵌入式 bridge，而非 shim。代价：需要 wails CLI、会打开
// 桌面窗口、构建较重——默认跳过，WOCA_E2E_NATIVE_DEV=1 时运行。
const WAILS = 'http://127.0.0.1:34115';

test.skip(!process.env.WOCA_E2E_NATIVE_DEV, 'set WOCA_E2E_NATIVE_DEV=1 (needs wails CLI; opens a window)');

test.describe('企业 wails 真机（wails dev + embeddedbridge）', () => {
  test('P0 双卡：真实 embeddedbridge 构建里本机模式与远程模式并存', async ({ page }) => {
    await pinLocale(page);
    await page.goto(WAILS);

    await expect(page.locator('[data-testid="embedded-entry"]')).toBeVisible({ timeout: 60_000 });
    await expect(page.locator('[data-testid="embedded-entry"]').getByText('本机模式')).toBeVisible();
    await expect(page.locator('[data-testid="remote-open"]')).toBeVisible();
    // 原生形态永远没有 web 登录门。
    await expect(page.locator(gate.overlay)).toHaveCount(0);
  });

  test('本机模式一键进入应用：真实内嵌 bridge 承载会话面', async ({ page }) => {
    await pinLocale(page);
    await page.goto(WAILS);

    await expect(page.locator('[data-testid="embedded-connect"]')).toBeVisible({ timeout: 60_000 });
    await page.locator('[data-testid="embedded-connect"]').click();

    // ConnectEmbedded 启动真内嵌 bridge（环回信任）→ 应用外壳就位。
    await expect(page.locator('aside.sidebar')).toBeVisible({ timeout: 60_000 });
    await expect(page.locator('.chat-input')).toBeVisible();
    await expect(page.locator(gate.overlay)).toHaveCount(0);
    // 内嵌 bridge 就是 woca-bridge：/api/status 为真 JSON（edition=bridge、
    // local 姿态）→ 状态灯存在。这与个人版 woca serve（status 落 SPA
    // HTML、无状态入口）恰成形态对照。
    await expect(page.locator('.status-lamp')).toBeVisible();
  });
});
