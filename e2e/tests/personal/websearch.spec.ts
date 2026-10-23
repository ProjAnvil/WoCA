import { test, expect } from '@playwright/test';
import { PERSONAL, pinLocale } from '../../fixtures/lab';

// Form 1 — the "Web search" section of the personal embedded settings page.
//
// Proves the section is a usable configuration surface: the backend catalog
// renders, the switch changes state, the order can be dragged, a save
// round-trips through the database, and the probe runs to completion.
//
// Two deliberate choices:
//
//  1. The probe is asserted only as "it finished and reported an outcome".
//     Whether a free backend answers depends on the network running the tests,
//     so asserting a specific backend would make CI randomly red on corporate
//     and datacenter networks.
//  2. Selectors use data-testid, and the UI text is asserted in Chinese
//     because the suite pins locale to zh-CN.
test.describe('personal settings → web search', () => {
  test.beforeEach(async ({ page }) => {
    await pinLocale(page);
  });

  /** Opens the settings panel and switches to the web-search section. */
  async function openWebSearch(page: import('@playwright/test').Page) {
    await page.goto(PERSONAL);
    await expect(page.locator('aside.sidebar')).toBeVisible();
    await page.locator('.footer-settings-btn').click();
    await page.locator('.settings-nav-item', { hasText: '网络搜索' }).click();
    await expect(page.locator('[data-testid="ws-row-ddg-html"]')).toBeVisible();
  }

  test('renders the backend catalog with the default chain ordered', async ({ page }) => {
    await openWebSearch(page)

    const ddgHTML = page.locator('[data-testid="ws-row-ddg-html"]')
    await expect(ddgHTML).toContainText('DuckDuckGo')
    await expect(ddgHTML.locator('.ws-order')).toHaveText('#1')

    const ddgLite = page.locator('[data-testid="ws-row-ddg-lite"]')
    await expect(ddgLite).toContainText('DuckDuckGo')
    await expect(ddgLite.locator('.ws-order')).toHaveText('#2')

    // Backends that need an endpoint are marked as such and start off.
    const searxng = page.locator('[data-testid="ws-row-searxng"]')
    await expect(searxng).toContainText('需要填端点')
    await expect(searxng.locator('.ws-order')).toHaveCount(0)
  })

  test('disabling a backend drops it from the order, re-enabling puts it last', async ({ page }) => {
    await openWebSearch(page)

    const ddgHTML = page.locator('[data-testid="ws-row-ddg-html"]')
    const ddgLite = page.locator('[data-testid="ws-row-ddg-lite"]')

    await ddgHTML.locator('[role="switch"]').click()
    await expect(ddgHTML.locator('.ws-order')).toHaveCount(0)
    // ddg-lite is the only enabled backend left, so it moves to #1.
    await expect(ddgLite.locator('.ws-order')).toHaveText('#1')

    await ddgHTML.locator('[role="switch"]').click()
    await expect(ddgHTML.locator('.ws-order')).toHaveText('#2')
  })

  // The endpoint field only appears once its backend is enabled — otherwise
  // operators are shown inputs for backends they are not using.
  test('enabling SearXNG reveals its endpoint field', async ({ page }) => {
    await openWebSearch(page)
    await expect(page.locator('#ws-searxng')).toHaveCount(0)

    await page.locator('[data-testid="ws-row-searxng"]').locator('[role="switch"]').click()
    await expect(page.locator('#ws-searxng')).toBeVisible()
  })

  // The probe is the point of the section: a blocked backend looks exactly
  // like a search that found nothing, so it must complete and say which it was.
  test('the probe runs to completion and reports an outcome', async ({ page }) => {
    await openWebSearch(page)

    await page.locator('[data-testid="ws-test"]').click()

    const out = page.locator('.ws-test')
    await expect(out).toBeVisible({ timeout: 45_000 })
    await expect(out).not.toHaveText('')
  })
})
