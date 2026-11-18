import fs from 'node:fs';
import http from 'node:http';
import { test, expect, type Page } from '@playwright/test';
import { pinLocale } from '../../fixtures/lab';
import { startDedicatedServe, stopDedicatedServe, type DedicatedServe } from '../../fixtures/dedicated';

// Form 1 — the session activity panel against the REAL `woca serve` binary.
// The model is a scripted OpenAI-compatible SSE stand-in served from the
// test process (the same wire shape cmd/woca's fakeOpenAIToolFlow pins):
// the first call of a conversation emits TodoWrite + two background Bash
// shells + ScheduleCron; any later call (tool results present) answers with
// plain text. Assertions use data-testid/class selectors and zh-CN strings
// (the suite pins locale to zh-CN). Screenshots land OUTSIDE the repo for
// the controller's visual review.
//
// Dedicated instance (the stopnotice/chatcolumn pattern): this spec
// reconfigures the provider AND the permission mode, both server-global on
// `woca serve` — the settings `provider` key is a single live selector with
// no per-run override, so on the shared personal webServer the hitl spec's
// identical setup raced this one for the slot (runs landing on the wrong
// scripted model or on a dead endpoint after the other spec's teardown
// reset; reproduced at HEAD with activity+hitl --repeat-each=5: 8/10
// failed). A private instance gives this spec its own provider slot,
// permission mode, sidebar and database — the same real binary and
// embedded web build under test.

const SHOTS = '/Users/yuhaochen/Documents/codebase/projanvil/woca-internal-docs/activity-panel-shots';

/** The tree-style shell the kill step terminates (AC9's process-tree shape).
 * Long enough that it cannot complete naturally before the kill step. */
const TREE_CMD = 'echo tree started; sleep 120 & sleep 120 & wait';
/** The long runner stays alive through the dark-theme shot at the end. */
const LONG_CMD = 'echo long runner up; sleep 600';
/** The first prompt identifies this spec's conversation on the shared model. */
const ARM_PROMPT = 'Arm the demo background work';

const TOOL_CALLS = [
  {
    name: 'TodoWrite',
    arguments: JSON.stringify({
      todos: [
        { content: 'Set up the demo workspace', status: 'completed' },
        { content: 'Run the background shells', status: 'in_progress', active_form: 'Running the background shells' },
        { content: 'Report the results', status: 'pending' },
      ],
    }),
  },
  { name: 'Bash', arguments: JSON.stringify({ command: TREE_CMD, run_in_background: true }) },
  { name: 'Bash', arguments: JSON.stringify({ command: LONG_CMD, run_in_background: true }) },
  { name: 'ScheduleCron', arguments: JSON.stringify({ name: 'daily-report', prompt: 'Summarize progress in one line.', cron: '0 3 * * *' }) },
];

let modelURL = '';
let modelRequests = 0;
let modelServer: http.Server | undefined;
let serve: DedicatedServe | undefined;
let base = '';

/** One SSE chunk in the chat-completions streaming shape. */
function chunk(delta: Record<string, unknown>, finish: string | null): string {
  return (
    'data: ' +
    JSON.stringify({
      id: 'chatcmpl-e2e',
      object: 'chat.completion.chunk',
      choices: [{ index: 0, delta, finish_reason: finish }],
    }) +
    '\n\n'
  );
}

function toolCallDelta() {
  return chunk(
    {
      role: 'assistant',
      tool_calls: TOOL_CALLS.map((c, i) => ({
        index: i,
        id: `call_${i + 1}`,
        type: 'function',
        function: c,
      })),
    },
    null,
  );
}

function finalDelta() {
  return (
    chunk({ role: 'assistant', content: 'All background work is armed.' }, null) + chunk({}, 'stop') + 'data: [DONE]\n\n'
  );
}

test.beforeAll(async () => {
  fs.mkdirSync(SHOTS, { recursive: true });
  // The scripted model: a tool-bearing conversation with no tool results yet
  // → arm the surface; anything else (tool results present, or a tool-less
  // call like the session-title summarizer) → final text. Non-streaming
  // callers (the title summarizer) get plain JSON, streamers get SSE. Only
  // THIS spec's conversation is counted (the dedicated instance runs only
  // this spec's conversations; a wake run of this session would show up in
  // the count).
  modelServer = http.createServer((req, res) => {
    const parts: Buffer[] = [];
    req.on('data', (b: Buffer) => parts.push(b));
    req.on('end', () => {
      let arm = false;
      let stream = true;
      let mine = false;
      try {
        const body = JSON.parse(Buffer.concat(parts).toString('utf8')) as {
          messages?: Array<{ role?: string; content?: string }>;
          tools?: unknown[];
          stream?: boolean;
        };
        stream = body.stream === true; // the title call omits the flag
        const msgs = body.messages ?? [];
        arm = (body.tools ?? []).length > 0 && !msgs.some((m) => m.role === 'tool');
        // Any message of THIS conversation — a wake run would carry the
        // history too, so it still counts (that is the point of D1).
        mine = msgs.some(
          (m) => (m.role === 'user' || m.role === 'human') && (m.content ?? '').includes(ARM_PROMPT),
        );
      } catch {
        arm = false;
      }
      if (mine) modelRequests++;
      if (!stream) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            id: 'chatcmpl-e2e',
            object: 'chat.completion',
            choices: [
              { index: 0, message: { role: 'assistant', content: 'Armed.' }, finish_reason: 'stop' },
            ],
          }),
        );
        return;
      }
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.end(arm ? toolCallDelta() + chunk({}, 'tool_calls') + 'data: [DONE]\n\n' : finalDelta());
    });
  });
  await new Promise<void>((resolve) => modelServer!.listen(0, '127.0.0.1', resolve));
  const addr = modelServer!.address();
  if (!addr || typeof addr !== 'object') throw new Error('fake model has no address');
  modelURL = `http://127.0.0.1:${addr.port}`;

  // The dedicated personal instance (fixtures/dedicated.ts): own provider
  // slot and permission mode, so no teardown reset is needed — the instance
  // dies with the worker and never strands a neighbor on a dead provider.
  serve = await startDedicatedServe('activity');
  base = serve.base;
});

test.afterAll(async () => {
  if (serve) {
    await stopDedicatedServe(serve);
  }
  if (modelServer) {
    await new Promise<void>((r) => modelServer!.close(() => r()));
  }
});

/** Opens the panel (it always opens on the Background tab) and returns the card. */
async function openPanel(page: Page) {
  await page.locator('.act-toggle').click();
  const card = page.locator('.act-pop-card');
  await expect(card).toBeVisible();
  await expect(card.locator('[data-tab="background"]')).toHaveAttribute('aria-selected', 'true');
  // The panel-pop enter transition runs ~320ms and Playwright's visibility
  // check passes at opacity 0 — let the fade settle before assertions and
  // screenshots read the panel.
  await page.waitForTimeout(500);
  return card;
}

/** Screenshots the popover card by clipping the page at its box. The card
 * is position:fixed (viewport box), while page-screenshot clips are
 * document-relative, so the window's scroll offset is added back. */
async function shootCard(page: Page, card: ReturnType<Page['locator']>, path: string) {
  const box = await card.boundingBox();
  if (!box) throw new Error(`no box for ${path}`);
  const scrollY = await page.evaluate(() => window.scrollY);
  await page.screenshot({ path, clip: { ...box, y: box.y + scrollY } });
}

test('activity panel: badge, tabs, kill (quiet), scheduled, narrow, dark', async ({ page }) => {
  test.setTimeout(180_000);

  // Point the dedicated instance's model at the scripted stand-in (loopback
  // trust, no gate); the rebuild applies from the next run. Permission mode
  // goes to bypass for this spec: the panel is the subject under test, and
  // the model's Bash calls would otherwise park the run on permission
  // prompts (the real user flow answers those in-chat).
  const put = await page.request.put(`${base}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status(), 'configure scripted model via settings').toBe(200);
  const perms = await page.request.post(`${base}/api/agent/permissions`, {
    data: { mode: 'bypassPermissions' },
  });
  expect(perms.status(), 'set bypass permission mode').toBe(200);

  await pinLocale(page);
  await page.goto(base);
  await expect(page.locator('aside.sidebar')).toBeVisible();
  // Start from a fresh session so this spec's conversation is exactly the
  // scripted one. Wait for the create response: the store switches its
  // active session only then, and sending earlier would drop the message
  // into the previously active session.
  const sessionCreated = page.waitForResponse(
    (r) => r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url()),
  );
  await page.getByRole('button', { name: '在此工作区新建会话' }).click();
  expect((await sessionCreated).status()).toBe(200);
  await expect(page.locator('.chat-input textarea')).toBeVisible();

  // Send the first message: the scripted model arms todos, two background
  // shells and a cron. Capture the session id from the run POST.
  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill(ARM_PROMPT);
  await page.keyboard.press('Enter');
  const sid = (await (await runPost).postDataJSON()).session_id as string;
  expect(sid).toBeTruthy();

  // The run completes: the final assistant text persists to the transcript
  // only when the run closes (mid-run it reads empty — persisted roles are
  // human/ai/tool), and the model saw exactly the two scripted calls.
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${base}/api/sessions/${sid}`);
        const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
        return messages.some((m) => (m.content ?? '').includes('All background work is armed.'));
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  // The arm round + the final call (the session-title summarizer may add a
  // tool-less call; the D1 baseline below is captured from this point on).
  expect(modelRequests).toBeGreaterThanOrEqual(2);
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${base}/api/sessions/${sid}/activity`);
        const snap = (await res.json()) as {
          todos: { items: unknown[] };
          shells: Array<{ command: string; status: string }>;
          crons: unknown[];
        };
        return (
          snap.todos.items.length === 3 &&
          snap.shells.filter((s) => s.status === 'running').length === 2 &&
          snap.crons.length === 1
        );
      },
      { timeout: 30_000 },
    )
    .toBe(true);

  // (a) Collapsed header: the badge counts the two running shells.
  const badge = page.locator('.act-toggle .w-icon-btn-badge');
  await expect(badge).toHaveText('2');
  await page.screenshot({ path: `${SHOTS}/01-header-badge.png`, clip: { x: 0, y: 0, width: 1280, height: 100 } });

  // (b) Panel open on Background (the default tab) with both shells running.
  let card = await openPanel(page);
  const rows = card.locator('.act-row');
  await expect(rows).toHaveCount(2);

  const treeRow = card.locator('.act-row', { hasText: TREE_CMD });
  await expect(treeRow.locator('.act-row-status')).toHaveText(/运行中/);
  await expect(card.locator('.act-row', { hasText: LONG_CMD }).locator('.act-row-status')).toHaveText(/运行中/);
  await shootCard(page, card, `${SHOTS}/02-panel-background-running.png`);

  // Kill the tree shell behind the confirm dialog (D1 wording). The popover
  // no longer closes on scroll (that listener is gone), so no re-opening
  // here — the only retry guard left is the 1s elapsed-time tick re-rendering
  // the row under the stop click, which can swallow it and leave the confirm
  // undialoged. Every assertion that can fail on that path sits inside the
  // retry's catch.
  const confirm = page.locator('.w-confirm');
  const row = card.locator('.act-row', { hasText: TREE_CMD });
  for (let attempt = 0; ; attempt++) {
    await row.locator('.act-stop-btn').click({ force: true, timeout: 5000 });
    try {
      await expect(confirm).toBeVisible({ timeout: 5000 });
    } catch (e) {
      if (attempt === 2) throw e;
      continue;
    }
    await expect(confirm.locator('.w-confirm-body')).toContainText(TREE_CMD);
    await confirm.locator('.w-confirm-actions button').nth(1).click(); // 终止
    await expect(row.locator('.act-row-status')).toHaveText(/由你停止/, { timeout: 10_000 });
    break;
  }

  // (c) After the kill: stopped by the user; the long runner keeps running.
  await expect(card.locator('.act-row', { hasText: LONG_CMD }).locator('.act-row-status')).toHaveText(/运行中/);
  await shootCard(page, card, `${SHOTS}/03-panel-background-after-kill.png`);

  // D1: the user stop never wakes the agent — no extra model call and no
  // auto-run pill within the settle window.
  const requestsAfterKill = modelRequests;
  await expect(card.locator('.pill-auto')).toHaveCount(0);
  await page.waitForTimeout(1500);
  expect(modelRequests, 'no wake run may call the model').toBe(requestsAfterKill);
  await expect(page.locator('.pill-auto')).toHaveCount(0);

  // (d) Todo tab: the three scripted items with their statuses.
  await card.locator('[data-tab="todo"]').click();
  const todoRows = card.locator('.act-todo-row');
  await expect(todoRows).toHaveCount(3);
  await expect(card.locator('.act-todo-progress')).toContainText('1/3');
  await shootCard(page, card, `${SHOTS}/04-panel-todo.png`);

  // (e) Scheduled tab: the cron entry with spec and prompt.
  await card.locator('[data-tab="scheduled"]').click();
  const cronRow = card.locator('.act-cron-row', { hasText: 'daily-report' });
  await expect(cronRow).toHaveCount(1);
  await expect(cronRow).toContainText('0 3 * * *');
  await expect(cronRow.locator('.act-cron-prompt')).toContainText('Summarize progress');
  await shootCard(page, card, `${SHOTS}/05-panel-scheduled.png`);

  // (f) Narrow viewport (≤640px): the popover stays an anchored popover and
  // inline targets reach 44×44. (openPanel re-selects the Background tab —
  // already the default — without clicking it: clicking the active tab
  // fights the entrance animation.)
  await page.setViewportSize({ width: 390, height: 844 });
  card = await openPanel(page); // resize closes the popover; reopen
  await expect(card.locator('.act-row').first()).toBeVisible();
  await shootCard(page, card, `${SHOTS}/06-panel-narrow.png`);
  await page.setViewportSize({ width: 1280, height: 720 });

  // (g) Dark theme variant of (b): the long runner is still alive. The
  // theme is the html.dark class (tokens.css; index.html applies the same
  // class from the persisted preference), so toggle it live — a reload
  // round-trip would churn the transcript and lose the open popover's state.
  await page.evaluate(() => {
    localStorage.setItem('woca-theme', 'dark');
    document.documentElement.classList.add('dark');
  });
  await page.waitForTimeout(700); // let the theme cross-fade settle
  card = await openPanel(page); // the resize back to 1280 closed it; reopen
  await expect(card.locator('.act-row', { hasText: LONG_CMD }).locator('.act-row-status')).toHaveText(/运行中/);
  await page.screenshot({ path: `${SHOTS}/07-panel-background-dark.png` });
});
