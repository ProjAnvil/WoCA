import http from 'node:http';
import { test, expect, request, type Page } from '@playwright/test';
import { PERSONAL, pinLocale } from '../../fixtures/lab';

// Form 1 — graph-native HITL against the REAL `woca serve` binary. The
// model is a scripted OpenAI-compatible SSE stand-in served from the test
// process (the same wire shape the activity spec pins). The scripted
// conversation fans TWO foreground Tasks out; each child's model call is
// scripted by its prompt marker (FILE-A / FILE-B) so both children pause on
// a gated call in ONE superstep — the multi-card approval stack, session
// grants, the deny path, and the Subagents tab are asserted end-to-end.

type Msg = { role: string; content?: string; tool_calls?: unknown[] };

let modelURL = '';
let modelRequests = 0;
let modelServer: http.Server | undefined;

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

function sse(res: http.ServerResponse, deltas: Record<string, unknown>[], finish: string | null) {
  res.writeHead(200, { 'content-type': 'text/event-stream' });
  for (const d of deltas) res.write(chunk(d, null));
  if (finish) res.write(chunk({}, finish));
  res.write('data: [DONE]\n\n');
  res.end();
}

function toolCall(id: string, name: string, args: Record<string, unknown>, index = 0) {
  return {
    index,
    id,
    type: 'function',
    function: { name, arguments: JSON.stringify(args) },
  };
}

/** The scripted model: parent turns are identified by the HITL-FANOUT
 * marker, child turns by FILE-A / FILE-B in the transcript. Children gated
 * call: Bash `npm install ...` (classifier non-readonly, grantable prefix)
 * for the grant scenario; Write for the plain allow/deny scenario. */
function scriptFor(messages: Msg[]): { deltas: Record<string, unknown>[]; finish: string } {
  const text = messages.map((m) => m.content ?? '').join('\n');
  const hasToolResults = messages.some((m) => m.role === 'tool');
  if (text.includes('FILE-B')) {
    if (hasToolResults) return { deltas: [{ content: 'child B done' }], finish: 'stop' };
    return {
      deltas: [{ role: 'assistant', tool_calls: [toolCall('cb1', 'Bash', { command: 'npm install left-pad' })] }],
      finish: 'tool_calls',
    };
  }
  if (text.includes('FILE-A')) {
    if (hasToolResults) return { deltas: [{ content: 'child A done' }], finish: 'stop' };
    return {
      deltas: [{ role: 'assistant', tool_calls: [toolCall('ca1', 'Bash', { command: 'npm install left-pad' })] }],
      finish: 'tool_calls',
    };
  }
  if (text.includes('HITL-FANOUT')) {
    if (hasToolResults) return { deltas: [{ content: 'Fan-out complete.' }], finish: 'stop' };
    return {
      deltas: [
        {
          role: 'assistant',
          tool_calls: [
            toolCall('tA', 'Task', { description: 'install A', prompt: 'FILE-A install the package' }, 0),
            toolCall('tB', 'Task', { description: 'install B', prompt: 'FILE-B install the package' }, 1),
          ],
        },
      ],
      finish: 'tool_calls',
    };
  }
  return { deltas: [{ content: 'unrelated' }], finish: 'stop' };
}

test.beforeAll(async () => {
  modelServer = http.createServer((req, res) => {
    if (!req.url?.includes('/chat/completions')) {
      res.writeHead(404).end();
      return;
    }
    const body: Uint8Array[] = [];
    req.on('data', (d) => body.push(d));
    req.on('end', () => {
      modelRequests++;
      const parsed = JSON.parse(Buffer.concat(body).toString()) as { messages: Msg[] };
      const script = scriptFor(parsed.messages ?? []);
      sse(res, script.deltas, script.finish);
    });
  });
  await new Promise<void>((r) => modelServer!.listen(0, '127.0.0.1', r));
  const addr = modelServer.address() as { port: number };
  modelURL = `http://127.0.0.1:${addr.port}`;
});

test.afterAll(async () => {
  if (modelServer) {
    const ctx = await request.newContext();
    await ctx
      .put(`${PERSONAL}/api/settings`, { data: { provider: 'anthropic', model: '' } })
      .catch(() => {});
    await ctx
      .post(`${PERSONAL}/api/agent/permissions`, { data: { mode: 'default' } })
      .catch(() => {});
    await ctx.dispose();
    await new Promise<void>((r) => modelServer!.close(() => r()));
  }
});

async function freshSession(page: Page): Promise<string> {
  const sessionCreated = page.waitForResponse(
    (r) => r.request().method() === 'POST' && /\/api\/sessions$/.test(r.url()),
  );
  await page.getByRole('button', { name: '在此工作区新建会话' }).click();
  expect((await sessionCreated).status()).toBe(200);
  await expect(page.locator('.chat-input textarea')).toBeVisible();
  const runPost = page.waitForRequest((r) => r.method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.locator('.chat-input textarea').fill('HITL-FANOUT run the installs');
  await page.keyboard.press('Enter');
  const sid = (await (await runPost).postDataJSON()).session_id as string;
  expect(sid).toBeTruthy();
  return sid;
}

async function configure(page: Page, mode: string) {
  const put = await page.request.put(`${PERSONAL}/api/settings`, {
    data: {
      provider: 'openai',
      model: 'gpt-e2e',
      openai_base_url: `${modelURL}/v1`,
      openai_api_key: 'e2e-key',
    },
  });
  expect(put.status(), 'configure scripted model via settings').toBe(200);
  const perms = await page.request.post(`${PERSONAL}/api/agent/permissions`, { data: { mode } });
  expect(perms.status(), `set ${mode} permission mode`).toBe(200);
}

async function sessionDone(page: Page, sid: string, marker: string) {
  await expect
    .poll(
      async () => {
        const res = await page.request.get(`${PERSONAL}/api/sessions/${sid}`);
        const { messages } = (await res.json()) as { messages: Array<{ content?: string }> };
        return messages.some((m) => (m.content ?? '').includes(marker));
      },
      { timeout: 45_000 },
    )
    .toBe(true);
}

test('hitl: two parallel child approvals, session grant, deny, subagents tab', async ({ page }) => {
  test.setTimeout(240_000);
  await configure(page, 'default');
  await pinLocale(page);
  await page.goto(PERSONAL);
  await expect(page.locator('aside.sidebar')).toBeVisible();
  const sid = await freshSession(page);

  // (1) Both children pause in one superstep: the stack renders TWO cards,
  // each a Bash npm-install ask from its own child.
  const stack = page.locator('[data-testid="hitl-stack"]');
  await expect(stack).toBeVisible({ timeout: 30_000 });
  const cards = stack.locator('[data-testid^="hitl-card-"]');
  await expect(cards).toHaveCount(2);

  // (2) Session grant on the FIRST card: the rule-key preview is visible
  // before answering; granting collapses the SIBLING card too (same
  // bash:npm * prefix — the second ask must not re-prompt).
  const first = cards.nth(0);
  const firstId = await first.getAttribute('data-testid');
  const second = cards.nth(1);
  const secondId = await second.getAttribute('data-testid');
  expect(firstId).not.toBe(secondId);
  await expect(first.locator(`[data-testid="${firstId!.replace('hitl-card-', 'hitl-rule-key-')}"]`)).toBeVisible();
  await first.locator(`[data-testid="${firstId!.replace('hitl-card-', 'hitl-grant-session-')}"]`).click();
  // The granted card resolves; the sibling's ask hits the fresh session
  // grant and executes without another card (the stack closes).
  await expect(stack).toBeHidden({ timeout: 20_000 });

  // (3) The run completes: both children ran their install and reported.
  await sessionDone(page, sid, 'Fan-out complete.');

  // (4) Grants management: the chip opens the panel listing the rule;
  // revoking removes the row (the next fan-out would ask again).
  const grantsChip = page.locator('[data-testid="grants-chip"]');
  await expect(grantsChip).toBeVisible();
  await grantsChip.click();
  const grantsPanel = page.locator('.grants-panel');
  await expect(grantsPanel).toBeVisible();
  const ruleRow = grantsPanel.locator('[data-testid^="grants-row-"]');
  await expect(ruleRow).toHaveCount(1);
  await expect(ruleRow.first()).toContainText('npm install');
  await ruleRow.first().locator('.grant-revoke').click();
  await expect(grantsPanel.locator('[data-testid^="grants-row-"]')).toHaveCount(0);
  await page.keyboard.press('Escape');

  // (5) Second fan-out in the same session (grants revoked): both cards
  // return; ALLOW one child, DENY the other — the denied child receives
  // the denial and still reports.
  await page.locator('.chat-input textarea').fill('HITL-FANOUT run the installs again');
  const runPost2 = page.waitForResponse((r) => r.request().method() === 'POST' && /\/api\/runs$/.test(r.url()));
  await page.keyboard.press('Enter');
  expect((await runPost2).status()).toBe(200);
  await expect(stack).toBeVisible({ timeout: 30_000 });
  const cards2 = stack.locator('[data-testid^="hitl-card-"]');
  await expect(cards2).toHaveCount(2);
  const allow = cards2.nth(0);
  const allowId = (await allow.getAttribute('data-testid'))!.replace('hitl-card-', '');
  await allow.locator(`[data-testid="hitl-allow-${allowId}"]`).click();
  // One card answered; the sibling re-pauses alone (partial answer).
  const cardsAfterAllow = stack.locator('[data-testid^="hitl-card-"]');
  await expect(cardsAfterAllow).toHaveCount(1, { timeout: 20_000 });
  const deny = cardsAfterAllow.nth(0);
  const denyId = (await deny.getAttribute('data-testid'))!.replace('hitl-card-', '');
  await deny.locator(`[data-testid="hitl-deny-${denyId}"]`).click();
  await expect(stack).toBeHidden({ timeout: 20_000 });
  await sessionDone(page, sid, 'Fan-out complete.Fan-out complete.');

  // (6) The Subagents tab: rows for the four child lifetimes, a terminal
  // status, and the readonly transcript expand.
  await page.locator('.act-toggle').click();
  const card = page.locator('.act-pop-card');
  await expect(card).toBeVisible();
  await card.locator('[data-testid="sub-tab"]').click();
  const rows = card.locator('[data-testid^="sub-row-"]');
  await expect(rows.first()).toBeVisible({ timeout: 15_000 });
  await expect(rows.count()).toBeGreaterThanOrEqual(2);
  const doneRow = rows.filter({ has: page.locator('[data-testid^="sub-status-"]', { hasText: /已完成|done/ }) }).first();
  await expect(doneRow).toBeVisible({ timeout: 15_000 });
  await doneRow.click();
  const transcript = doneRow.locator('[data-testid^="sub-transcript-"]');
  await expect(transcript).toBeVisible();

  // (7) Background tab initiator column: shells started by children (the
  // installs are foreground; this asserts the column + main attribution on
  // whatever the session shows — the earlier activity spec's data may share
  // the server, so scope to THIS session's snapshot having the column).
  await card.locator('[data-tab="background"]').click();
  await expect(card.locator('[data-testid="initiator-col"]')).toBeVisible();
});
