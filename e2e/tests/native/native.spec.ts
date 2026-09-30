import { test, expect } from '@playwright/test';
import { STATIC, gate, pinLocale } from '../../fixtures/lab';
import { installNativeShim, nativeCalls } from '../../fixtures/native-shim';

// Form 3 — 企业版，员工使用 wails 连接（原生连接向导）。
// 浏览器无法承载 Wails v2 进程（无 headless 模式），因此注入忠实实现
// NativeBridge 契约的 window.go shim，驱动真实构建产物里的真实
// NativeGate/ConnectView 向导——被测的是用户担心的那块 UI 本身：
// 个人（本机模式）与企业（远程模式）连接卡片绝不允许混淆。
async function openWizard(page: import('@playwright/test').Page, cfg: Parameters<typeof installNativeShim>[1] = {}) {
  await pinLocale(page);
  await installNativeShim(page, cfg);
  await page.goto(STATIC);
  await expect(page.locator('[data-testid="remote-open"]')).toBeVisible({ timeout: 20_000 });
}

test.describe('企业 wails 原生向导（ConnectView）', () => {
  test('P0 双卡并存：本机模式（embedded-entry）与远程模式（remote-open）', async ({ page }) => {
    await openWizard(page, { embeddedUrl: 'http://127.0.0.1:3490' });

    const embedded = page.locator('[data-testid="embedded-entry"]');
    await expect(embedded).toBeVisible();
    await expect(embedded.getByText('本机模式')).toBeVisible();
    await expect(page.locator('[data-testid="embedded-connect"]')).toBeVisible();

    const remote = page.locator('[data-testid="remote-open"]');
    await expect(remote).toBeVisible();
    await expect(remote.getByText('远程模式')).toBeVisible();

    // 原生形态的反向金丝雀：绝不出现 web 登录门。
    await expect(page.locator(gate.overlay)).toHaveCount(0);
    await expect(page.locator('aside.sidebar')).toHaveCount(0);
  });

  test('P0 无内嵌 bridge 的构建：本机模式卡缺席，仅远程模式', async ({ page }) => {
    await openWizard(page, {}); // 无 embeddedUrl

    await expect(page.locator('[data-testid="remote-open"]')).toBeVisible();
    await expect(page.locator('[data-testid="embedded-entry"]')).toHaveCount(0);
  });

  test('P0 已保存连接：过期档案入静默清单，活跃档案成续连卡', async ({ page }) => {
    const now = Date.now();
    await openWizard(page, {
      embeddedUrl: 'http://127.0.0.1:3490',
      profiles: [
        { id: 7, server_url: 'https://edge.corp', bridge_id: 'br_a', session_state: 'active', last_used_at: now, active: true },
        { id: 8, server_url: 'http://10.0.0.5:3490', session_state: 'signed_out', last_used_at: now - 86400_000, active: false },
      ],
    });

    await expect(page.locator('[data-testid="saved-profiles"]')).toBeVisible();
    // 活跃档案 → hero 续连卡：已登录徽标 + 一键「继续使用」。
    await expect(page.locator('.gate-profile-badge.active', { hasText: '已登录' })).toBeVisible();
    await expect(page.getByText('继续使用')).toBeVisible();
    // 静默清单列出过期档案。
    await expect(page.getByText('http://10.0.0.5:3490')).toBeVisible();
  });

  test('P1 路线卡：连接私有云 与 直连 bridge 两条入口', async ({ page }) => {
    await openWizard(page, { embeddedUrl: 'http://127.0.0.1:3490' });
    await page.locator('[data-testid="remote-open"]').click();

    await expect(page.locator('[data-testid="tab-private"]')).toBeVisible();
    await expect(page.locator('[data-testid="tab-private"]').getByText('连接私有云')).toBeVisible();
    await expect(page.locator('[data-testid="tab-direct"]').getByText('直连 bridge')).toBeVisible();
    // 有内嵌 bridge 且未上云 → P1 提供注册入口（上云引导）。
    await expect(page.locator('[data-testid="embedded-enroll-open"]')).toBeVisible();
  });

  test('P2A 私有云表单：空提交与非法地址被本地校验拦截', async ({ page }) => {
    await openWizard(page, {
      bridges: [
        { bridge_id: 'br_alpha', live: true },
        { bridge_id: 'br_beta', live: true },
      ],
    });
    await page.locator('[data-testid="remote-open"]').click();
    await page.locator('[data-testid="tab-private"]').click();

    // 空提交 → 校验提示。
    await page.locator('.gate-submit').click();
    await expect(page.locator('[data-testid="connection-validation"]')).toBeVisible();

    // 非法地址（非 http(s) origin）→ 校验提示。
    await page.locator('[data-testid="url-input"]').fill('not-a-url');
    await page.locator('[data-testid="api-key-input"]').fill('woca_shim_key');
    await page.locator('.gate-submit').click();
    await expect(page.locator('[data-testid="connection-validation"]')).toBeVisible();
  });

  test('P3 选桥：多 bridge 进选择页，点选后 Connect 携带 bridge_id', async ({ page }) => {
    await openWizard(page, {
      bridges: [
        { bridge_id: 'br_alpha', live: true },
        { bridge_id: 'br_beta', live: true },
      ],
    });
    await page.locator('[data-testid="remote-open"]').click();
    await page.locator('[data-testid="tab-private"]').click();
    await page.locator('[data-testid="url-input"]').fill('http://127.0.0.1:13712');
    await page.locator('[data-testid="api-key-input"]').fill('woca_shim_key');
    await page.locator('.gate-submit').click();

    await expect(page.locator('[data-testid="bridge-step"]')).toBeVisible();
    await expect(page.locator('.gate-bridge')).toHaveCount(2);

    await page.locator('.gate-bridge').first().click();
    const calls = await nativeCalls(page);
    const connect = calls.find(([name]) => name === 'Connect');
    expect(connect).toBeTruthy();
    expect(connect![1]).toMatchObject({
      server_url: 'http://127.0.0.1:13712',
      api_key: 'woca_shim_key',
      bridge_id: 'br_alpha',
    });
    // 连接完成后向导退场。
    await expect(page.locator('[data-testid="remote-open"]')).toHaveCount(0);
  });

  test('P4/P5 注册与单密钥登录（未上云形态）', async ({ page }) => {
    const now = Date.now();
    await installNativeShim(page, {
      embeddedUrl: 'http://127.0.0.1:3490',
      enrolled: false,
      // 注册成功但 user key 被核心拒绝 → 向导落 P5 单密钥登录。
      enrollError: 'user_key',
      profiles: [{ id: 1, server_url: 'https://edge.corp', session_state: 'expired', last_used_at: now, active: false }],
    });
    await pinLocale(page);
    await page.goto(STATIC);
    await expect(page.locator('[data-testid="remote-open"]')).toBeVisible({ timeout: 20_000 });

    await page.locator('[data-testid="remote-open"]').click();
    const enrollOpen = page.locator('[data-testid="embedded-enroll-open"]');
    await expect(enrollOpen).toBeVisible();
    await enrollOpen.click();

    await expect(page.locator('[data-testid="enroll-step"]')).toBeVisible();
    await expect(page.locator('[data-testid="enroll-url"]')).toBeVisible();
    await expect(page.locator('[data-testid="enroll-token"]')).toBeVisible();
    await expect(page.locator('[data-testid="enroll-key"]')).toBeVisible();

    // token 必须带 woca_enroll_ 前缀。
    await page.locator('[data-testid="enroll-url"]').fill('http://127.0.0.1:13712');
    await page.locator('[data-testid="enroll-token"]').fill('wrong_token');
    await page.locator('[data-testid="enroll-key"]').fill('woca_user_key');
    await page.locator('[data-testid="enroll-submit"]').click();
    await expect(page.locator('[data-testid="enroll-validation"]')).toBeVisible();

    // 合法提交 → 注册成功但 user_key 被拒 → P5 单密钥登录页。
    await page.locator('[data-testid="enroll-token"]').fill('woca_enroll_shim');
    await page.locator('[data-testid="enroll-submit"]').click();
    await expect(page.locator('[data-testid="enroll-login-step"]')).toBeVisible();
    await expect(page.locator('[data-testid="enroll-login-key"]')).toBeVisible();

    // P5 登录提交 → Connect 携带 edge 地址与密钥。
    await page.locator('[data-testid="enroll-login-key"]').fill('woca_user_key');
    await page.locator('[data-testid="enroll-login-submit"]').click();
    const calls = await nativeCalls(page);
    const connect = calls.find(([name]) => name === 'Connect');
    expect(connect).toBeTruthy();
    expect(connect![1]).toMatchObject({ server_url: 'http://127.0.0.1:13712', api_key: 'woca_user_key' });
  });
});
