import fs from 'node:fs';
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { startDedicatedServe, stopDedicatedServe, type DedicatedServe } from '../../fixtures/dedicated';
import { runtimeDir } from '../../fixtures/paths';

// F6 tablet rail smoke (spec 2026-11-19-tablet-icon-rail §3): at the 768
// and 1023 band edges the left sidebar folds into the 56px rail and the
// chat keeps viewport − 56; the sessions flyout opens against the scrim,
// closes via the rail toggle (the real z21 hit-test — the button must sit
// above the veil), via Esc, and on session selection.
//
// Dedicated instance for the same reason as stopnotice.spec.ts: the
// workspace seeded here would add a sidebar group on the shared personal
// server and break the neighbors' one-group locators.

let serve: DedicatedServe | undefined;
let base = '';

test.beforeAll(async () => {
  serve = await startDedicatedServe('rail');
  base = serve.base;
});

test.afterAll(async () => {
  if (serve) await stopDedicatedServe(serve);
});

/** Box widths of the rail and the chat column at the current viewport. */
async function shellBoxes(page: Page): Promise<{ sidebar: number; main: number; viewport: number }> {
  return page.evaluate(() => {
    const sidebar = document.querySelector('#app-sidebar') as HTMLElement | null;
    const main = document.querySelector('.main') as HTMLElement | null;
    if (!sidebar || !main) throw new Error('shell columns missing');
    return {
      sidebar: sidebar.getBoundingClientRect().width,
      main: main.getBoundingClientRect().width,
      viewport: window.innerWidth,
    };
  });
}

test('tablet band: 56px rail keeps the chat; the flyout opens and closes', async ({ page }) => {
  test.setTimeout(120_000);

  // One workspace + session so the flyout's list has a row to select.
  const ws = path.join(runtimeDir, 'rail', `ws-${process.pid}-${Date.now()}`);
  fs.mkdirSync(ws, { recursive: true });
  const seed = await page.request.post(`${base}/api/sessions`, { data: { workspace: ws } });
  expect(seed.status(), 'seed a session in the rail workspace').toBe(200);

  await pinLocale(page);
  await page.setViewportSize({ width: 768, height: 1024 });
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();

  const sessionsBtn = page.locator('.rail-btn[title="会话"]');
  const flyout = page.locator('#rail-flyout');
  const scrim = page.locator('.flyout-scrim');

  for (const width of [768, 1023]) {
    await page.setViewportSize({ width, height: 1024 });

    // The rail is the shell's left column; the 280px column content is
    // gone and the chat keeps everything past the rail.
    await expect(page.locator('.rail-nav')).toBeVisible();
    await expect(page.locator('#app-sidebar .sidebar-body')).toHaveCount(0);
    const boxes = await shellBoxes(page);
    console.log(`${width} viewport shell widths`, JSON.stringify(boxes));
    expect(boxes.sidebar, `${width}: the rail is 56px`).toBe(56);
    // The rail's 1px border may eat into the chat's box; the column keeps
    // viewport − 56 within that border.
    expect(Math.abs(boxes.main - (width - 56)), `${width}: the chat keeps viewport − 56`).toBeLessThanOrEqual(1);

    // Open: flyout + scrim; the sessions button reports expanded.
    await sessionsBtn.click();
    await expect(flyout).toBeVisible();
    await expect(scrim).toBeVisible();
    await expect(sessionsBtn).toHaveAttribute('aria-expanded', 'true');

    // The active indicator (spec D2): a 3px bar on the rail's left edge.
    // The button centers 5.5px into the 55px content box (56 − 1px
    // border), so the pseudo-element must sit at left ≈ −5.5px to land on
    // the rail's edge — inside the shell's overflow:hidden (the
    // prototype's own −10px never rendered; final-review minor).
    const bar = await sessionsBtn.evaluate((el) => {
      const cs = getComputedStyle(el, '::before')
      return { left: cs.left, width: cs.width, background: cs.backgroundColor }
    });
    expect(parseFloat(bar.left), `${width}: the ink bar anchors at the rail edge`).toBeCloseTo(-5.5, 1);
    expect(parseFloat(bar.width), `${width}: the ink bar is 3px`).toBe(3);
    expect(bar.background, `${width}: the ink bar paints (accent)`).not.toBe('rgba(0, 0, 0, 0)');

    // The z21 anchor, proven by a real hit-test: the button sits above
    // the fullscreen veil, so clicking it again closes the flyout.
    await sessionsBtn.click();
    await expect(flyout).toHaveCount(0);

    // Esc closes.
    await sessionsBtn.click();
    await expect(flyout).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(flyout).toHaveCount(0);
    await expect(scrim).toHaveCount(0);

    // Selecting a session closes the flyout (drawer convention).
    await sessionsBtn.click();
    await expect(flyout).toBeVisible();
    await page.locator('[data-testid="session-item"]').first().click();
    await expect(flyout).toHaveCount(0);
    await expect(scrim).toHaveCount(0);
  }

  // Past the band the static 280px column returns.
  await page.setViewportSize({ width: 1024, height: 1024 });
  await expect(page.locator('.rail-nav')).toHaveCount(0);
  await expect(page.locator('#app-sidebar .sidebar-body')).toBeVisible();
  const wide = await shellBoxes(page);
  expect(wide.sidebar, '1024: the 280px column returns').toBe(280);
});
